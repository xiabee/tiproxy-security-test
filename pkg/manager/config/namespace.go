// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"encoding/json"
	"path"
	"strings"

	"github.com/pingcap/tiproxy/lib/config"
	"github.com/pingcap/tiproxy/lib/util/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (e *ConfigManager) get(ctx context.Context, ns, key string) (KVValue, error) {
	nkey := path.Clean(path.Join(ns, key))
	v, ok := e.kv.Get(KVValue{Key: nkey})
	if !ok {
		return v, errors.WithStack(errors.Wrapf(ErrNoResults, "key=%s", nkey))
	}
	return v, nil
}

func (e *ConfigManager) list(ctx context.Context, ns string, ops ...clientv3.OpOption) ([]KVValue, error) {
	k := path.Clean(ns)
	var resp []KVValue
	e.kv.Ascend(KVValue{Key: k}, func(item KVValue) bool {
		if !strings.HasPrefix(item.Key, k) {
			return false
		}
		resp = append(resp, item)
		return true
	})
	return resp, nil
}

func (e *ConfigManager) set(ctx context.Context, ns, key string, val []byte) error {
	v := KVValue{Key: path.Clean(path.Join(ns, key)), Value: val}
	_, _ = e.kv.Set(v)
	return nil
}

func (e *ConfigManager) del(ctx context.Context, ns, key string) error {
	_, _ = e.kv.Delete(KVValue{Key: path.Clean(path.Join(ns, key))})
	return nil
}

func (e *ConfigManager) GetNamespace(ctx context.Context, ns string) (*config.Namespace, error) {
	kv, err := e.get(ctx, pathPrefixNamespace, ns)
	if err != nil {
		return nil, err
	}
	var cfg config.Namespace
	err = json.Unmarshal(kv.Value, &cfg)
	return &cfg, err
}

func (e *ConfigManager) ListAllNamespace(ctx context.Context) ([]*config.Namespace, error) {
	etcdKeyValues, err := e.list(ctx, pathPrefixNamespace)
	if err != nil {
		return nil, err
	}

	var ret []*config.Namespace
	for _, kv := range etcdKeyValues {
		var nsCfg config.Namespace
		if err := json.Unmarshal(kv.Value, &nsCfg); err != nil {
			return nil, err
		}
		ret = append(ret, &nsCfg)
	}

	return ret, nil
}

func (e *ConfigManager) SetNamespace(ctx context.Context, ns string, nsc *config.Namespace) error {
	if ns == "" || nsc.Namespace == "" {
		return errors.New("namespace name can not be empty string")
	}
	r, err := json.Marshal(nsc)
	if err != nil {
		return err
	}
	return e.set(ctx, pathPrefixNamespace, ns, r)
}

func (e *ConfigManager) DelNamespace(ctx context.Context, ns string) error {
	return e.del(ctx, pathPrefixNamespace, ns)
}
