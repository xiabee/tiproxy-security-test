// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package net

import "fmt"

type Command byte

// Command information. Ref https://dev.mysql.com/doc/dev/mysql-server/latest/my__command_8h.html#ae2ff1badf13d2b8099af8b47831281e1.
const (
	ComSleep Command = iota
	ComQuit
	ComInitDB
	ComQuery
	ComFieldList
	ComCreateDB
	ComDropDB
	ComRefresh
	ComDeprecated1
	ComStatistics
	ComProcessInfo
	ComConnect
	ComProcessKill
	ComDebug
	ComPing
	ComTime
	ComDelayedInsert
	ComChangeUser
	ComBinlogDump
	ComTableDump
	ComConnectOut
	ComRegisterSlave
	ComStmtPrepare
	ComStmtExecute
	ComStmtSendLongData
	ComStmtClose
	ComStmtReset
	ComSetOption
	ComStmtFetch
	ComDaemon
	ComBinlogDumpGtid
	ComResetConnection
	ComEnd // Not a real command
)

// Ref https://github.com/pingcap/tidb/blob/master/server/metrics/metrics.go#L51. Should be same.
var commandStrs = [ComEnd]string{
	"Sleep",
	"Quit",
	"InitDB",
	"Query",
	"FieldList",
	"CreateDB",
	"DropDB",
	"Refresh",
	"(DEPRECATED)Shutdown",
	"Statistics",
	"ProcessInfo",
	"Connect",
	"ProcessKill",
	"Debug",
	"Ping",
	"Time",
	"DelayedInsert",
	"ChangeUser",
	"BinlogDump",
	"TableDump",
	"ConnectOut",
	"RegisterSlave",
	"StmtPrepare",
	"StmtExecute",
	"StmtSendLongData",
	"StmtClose",
	"StmtReset",
	"SetOption",
	"StmtFetch",
	"Daemon",
	"BinlogDumpGtid",
	"ResetConnect",
}

func (f Command) Byte() byte {
	return byte(f)
}

func (f Command) String() string {
	e := int(f)
	if e >= len(commandStrs) {
		return fmt.Sprintf("Not a command: %x", byte(f))
	}
	return commandStrs[e]
}

func (f *Command) MarshalText() ([]byte, error) {
	return []byte(f.String()), nil
}

func (f *Command) UnmarshalText(o []byte) error {
	for e, c := range commandStrs {
		if c == string(o) {
			*f = Command(e)
			break
		}
	}
	return nil
}
