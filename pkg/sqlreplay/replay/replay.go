// Copyright 2024 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"context"
	"crypto/tls"
	"io"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/pingcap/tiproxy/lib/util/errors"
	"github.com/pingcap/tiproxy/lib/util/waitgroup"
	"github.com/pingcap/tiproxy/pkg/proxy/backend"
	pnet "github.com/pingcap/tiproxy/pkg/proxy/net"
	"github.com/pingcap/tiproxy/pkg/sqlreplay/cmd"
	"github.com/pingcap/tiproxy/pkg/sqlreplay/conn"
	"github.com/pingcap/tiproxy/pkg/sqlreplay/report"
	"github.com/pingcap/tiproxy/pkg/sqlreplay/store"
	"go.uber.org/zap"
)

const (
	maxPendingExceptions = 1024 // pending exceptions for all connections
	minSpeed             = 0.1
	maxSpeed             = 10.0
)

type Replay interface {
	// Start starts the replay
	Start(cfg ReplayConfig, backendTLSConfig *tls.Config, hsHandler backend.HandshakeHandler, bcConfig *backend.BCConfig) error
	// Stop stops the replay
	Stop(err error)
	// Progress returns the progress of the replay job
	Progress() (float64, error)
	// Close closes the replay
	Close()
}

type ReplayConfig struct {
	Input    string
	Username string
	Password string
	Speed    float64
	// the following fields are for testing
	reader      cmd.LineReader
	report      report.Report
	connCreator conn.ConnCreator
}

func (cfg *ReplayConfig) Validate() error {
	if cfg.Input == "" {
		return errors.New("input is required")
	}
	st, err := os.Stat(cfg.Input)
	if err == nil {
		if !st.IsDir() {
			return errors.New("output should be a directory")
		}
	} else {
		return errors.WithStack(err)
	}
	if cfg.Username == "" {
		return errors.New("username is required")
	}
	if cfg.Speed == 0 {
		cfg.Speed = 1
	} else if cfg.Speed < minSpeed || cfg.Speed > maxSpeed {
		return errors.Errorf("speed should be between %f and %f", minSpeed, maxSpeed)
	}
	return nil
}

type replay struct {
	sync.Mutex
	cfg              ReplayConfig
	meta             store.Meta
	conns            map[uint64]conn.Conn
	exceptionCh      chan conn.Exception
	closeCh          chan uint64
	wg               waitgroup.WaitGroup
	cancel           context.CancelFunc
	connCreator      conn.ConnCreator
	report           report.Report
	err              error
	startTime        time.Time
	endTime          time.Time
	progress         float64
	replayedCmds     uint64
	filteredCmds     uint64
	connCount        int
	backendTLSConfig *tls.Config
	lg               *zap.Logger
}

func NewReplay(lg *zap.Logger) *replay {
	return &replay{
		lg: lg,
	}
}

func (r *replay) Start(cfg ReplayConfig, backendTLSConfig *tls.Config, hsHandler backend.HandshakeHandler, bcConfig *backend.BCConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	r.Lock()
	defer r.Unlock()
	r.cfg = cfg
	r.meta = *r.readMeta()
	r.startTime = time.Now()
	r.endTime = time.Time{}
	r.progress = 0
	r.conns = make(map[uint64]conn.Conn)
	r.exceptionCh = make(chan conn.Exception, maxPendingExceptions)
	r.closeCh = make(chan uint64, maxPendingExceptions)
	hsHandler = NewHandshakeHandler(hsHandler)
	r.connCreator = cfg.connCreator
	if r.connCreator == nil {
		r.connCreator = func(connID uint64) conn.Conn {
			return conn.NewConn(r.lg.Named("conn"), r.cfg.Username, r.cfg.Password, backendTLSConfig, hsHandler, connID, bcConfig, r.exceptionCh, r.closeCh)
		}
	}
	r.report = cfg.report
	if r.report == nil {
		backendConnCreator := func() conn.BackendConn {
			// TODO: allocate connection ID.
			return conn.NewBackendConn(r.lg.Named("be"), 1, hsHandler, bcConfig, backendTLSConfig, r.cfg.Username, r.cfg.Password)
		}
		r.report = report.NewReport(r.lg.Named("report"), r.exceptionCh, backendConnCreator)
	}

	childCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	if err := r.report.Start(childCtx, report.ReportConfig{
		TlsConfig: r.backendTLSConfig,
	}); err != nil {
		return err
	}
	r.wg.RunWithRecover(func() {
		r.readCommands(childCtx)
	}, nil, r.lg)
	r.wg.RunWithRecover(func() {
		r.readCloseCh(childCtx)
	}, nil, r.lg)
	return nil
}

func (r *replay) readCommands(ctx context.Context) {
	// cfg.reader is set in tests
	reader := r.cfg.reader
	if reader == nil {
		reader = store.NewLoader(r.lg.Named("loader"), store.LoaderCfg{
			Dir: r.cfg.Input,
		})
	}
	defer reader.Close()

	var captureStartTs, replayStartTs time.Time
	for ctx.Err() == nil {
		command := &cmd.Command{}
		if err := command.Decode(reader); err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			r.Stop(err)
			break
		}
		// Replayer always uses the same username. It has no passwords for other users.
		// TODO: clear the session states.
		if command.Type == pnet.ComChangeUser {
			r.filteredCmds++
			continue
		}
		if captureStartTs.IsZero() {
			// first command
			captureStartTs = command.StartTs
			replayStartTs = time.Now()
		} else {
			expectedInterval := command.StartTs.Sub(captureStartTs)
			if r.cfg.Speed != 1 {
				expectedInterval = time.Duration(float64(expectedInterval) / r.cfg.Speed)
			}
			curInterval := time.Since(replayStartTs)
			if curInterval+time.Microsecond < expectedInterval {
				select {
				case <-ctx.Done():
				case <-time.After(expectedInterval - curInterval):
				}
			}
		}
		if ctx.Err() != nil {
			break
		}
		r.replayedCmds++
		r.executeCmd(ctx, command)
	}
}

func (r *replay) executeCmd(ctx context.Context, command *cmd.Command) {
	r.Lock()
	defer r.Unlock()

	conn, ok := r.conns[command.ConnID]
	if !ok {
		conn = r.connCreator(command.ConnID)
		r.conns[command.ConnID] = conn
		r.connCount++
		r.wg.RunWithRecover(func() {
			conn.Run(ctx)
		}, nil, r.lg)
	}
	if conn != nil && !reflect.ValueOf(conn).IsNil() {
		conn.ExecuteCmd(command)
	}
}

func (r *replay) readCloseCh(ctx context.Context) {
	// Drain all close events even if the context is canceled.
	// Otherwise, the connections may block at writing to channels.
	for {
		if ctx.Err() != nil {
			r.Lock()
			connCount := r.connCount
			r.Unlock()
			if connCount <= 0 {
				return
			}
		}
		select {
		case c, ok := <-r.closeCh:
			if !ok {
				return
			}
			// Keep the disconnected connections in the map to reject subsequent commands with the same connID,
			// but release memory as much as possible.
			r.Lock()
			if conn, ok := r.conns[c]; ok && conn != nil && !reflect.ValueOf(conn).IsNil() {
				r.conns[c] = nil
				r.connCount--
			}
			r.Unlock()
		case <-time.After(100 * time.Millisecond):
			// If context is canceled now but no connection exists, it will block forever.
			// Check the context and connCount again.
		}
	}
}

func (r *replay) Progress() (float64, error) {
	r.Lock()
	defer r.Unlock()
	if r.meta.Cmds > 0 {
		r.progress = float64(r.replayedCmds+r.filteredCmds) / float64(r.meta.Cmds)
	}
	return r.progress, r.err
}

func (r *replay) readMeta() *store.Meta {
	m := new(store.Meta)
	if err := m.Read(r.cfg.Input); err != nil {
		r.lg.Error("read meta failed", zap.Error(err))
	}
	return m
}

func (r *replay) Stop(err error) {
	r.Lock()
	defer r.Unlock()
	// already stopped
	if r.startTime.IsZero() {
		return
	}
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}

	r.endTime = time.Now()
	fields := []zap.Field{
		zap.Time("start_time", r.startTime),
		zap.Time("end_time", r.endTime),
		zap.Uint64("replayed_cmds", r.replayedCmds),
	}
	if err != nil {
		r.err = err
		if r.meta.Cmds > 0 {
			r.progress = float64(r.replayedCmds+r.filteredCmds) / float64(r.meta.Cmds)
			fields = append(fields, zap.Float64("progress", r.progress))
		}
		fields = append(fields, zap.Error(err))
		r.lg.Error("replay failed", fields...)
	} else {
		r.progress = 1
		fields = append(fields, zap.Float64("progress", r.progress))
		r.lg.Info("replay finished", fields...)
	}
	r.startTime = time.Time{}
}

func (r *replay) Close() {
	r.Stop(errors.New("shutting down"))
	r.wg.Wait()
}
