// Copyright 2024 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package vip

import (
	"runtime"

	"github.com/j-keck/arping"
	"github.com/pingcap/tiproxy/lib/util/errors"
	"github.com/vishvananda/netlink"
)

// NetworkOperation is the interface for adding, deleting, and broadcasting VIP.
// Extract the operations into an interface to make testing easier.
type NetworkOperation interface {
	HasIP() (bool, error)
	AddIP() error
	DeleteIP() error
	SendARP() error
}

var _ NetworkOperation = (*networkOperation)(nil)

type networkOperation struct {
	// the VIP address
	address *netlink.Addr
	// the network interface
	link netlink.Link
}

func NewNetworkOperation(addressStr, linkStr string) (NetworkOperation, error) {
	no := &networkOperation{}
	if err := no.initAddr(addressStr, linkStr); err != nil {
		return nil, err
	}
	return no, nil
}

func (no *networkOperation) initAddr(addressStr, linkStr string) error {
	if runtime.GOOS != "linux" {
		return errors.New("VIP is only supported on Linux")
	}
	address, err := netlink.ParseAddr(addressStr)
	if err != nil {
		return errors.WithStack(err)
	}
	no.address = address
	link, err := netlink.LinkByName(linkStr)
	if err != nil {
		return errors.WithStack(err)
	}
	no.link = link
	return nil
}

func (no *networkOperation) HasIP() (bool, error) {
	addresses, err := netlink.AddrList(no.link, 0)
	if err != nil {
		return false, errors.WithStack(err)
	}
	for _, addr := range addresses {
		if addr.Equal(*no.address) {
			return true, nil
		}
	}
	return false, nil
}

func (no *networkOperation) AddIP() error {
	return netlink.AddrAdd(no.link, no.address)
}

func (no *networkOperation) DeleteIP() error {
	return netlink.AddrDel(no.link, no.address)
}

func (no *networkOperation) SendARP() error {
	return arping.GratuitousArpOverIfaceByName(no.address.IP, no.link.Attrs().Name)
}
