// Copyright 2023 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package proxyprotocol

import "net"

type ProxyVersion int

const (
	ProxyVersion2 ProxyVersion = iota + 2
)

type ProxyCommand int

const (
	ProxyCommandLocal ProxyCommand = iota
	ProxyCommandProxy
)

type ProxyAddressFamily int

const (
	ProxyAFUnspec ProxyAddressFamily = iota
	ProxyAFINet
	ProxyAFINet6
	ProxyAFUnix
)

type ProxyNetwork int

const (
	ProxyNetworkUnspec ProxyNetwork = iota
	ProxyNetworkStream
	ProxyNetworkDgram
)

type ProxyTlvType int

const (
	ProxyTlvALPN ProxyTlvType = iota + 0x01
	ProxyTlvAuthority
	ProxyTlvCRC32C
	ProxyTlvNoop
	ProxyTlvUniqueID
	ProxyTlvSSL ProxyTlvType = iota + 0x20
	ProxyTlvSSLCN
	ProxyTlvSSLCipher
	ProxyTlvSSLSignALG
	ProxyTlvSSLKeyALG
	ProxyTlvNetns ProxyTlvType = iota + 0x30
)

type ProxyTlv struct {
	Content []byte
	Typ     ProxyTlvType
}

type Proxy struct {
	SrcAddress net.Addr
	DstAddress net.Addr
	TLV        []ProxyTlv
	Version    ProxyVersion
	Command    ProxyCommand
}

type AddressWrapper interface {
	net.Addr
	Unwrap() net.Addr
}
