package dnp3_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
	"github.com/dscsystems/go-dnp3/secevent"
)

func TestMasterReportsRefusedAuthentication(t *testing.T) {
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1,
		SecureAuthentication: &outstation.SecureAuthenticationConfig{
			Users: map[uint16][]byte{1: bytes.Repeat([]byte{0x11}, 16)}}},
		channel.TCPServer("127.0.0.1:0"))

	log := newEventLog()
	m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, Security: log.policy(),
		SecureAuthentication: &master.SecureAuthenticationConfig{User: 1, UpdateKey: bytes.Repeat([]byte{0x22}, 16)}}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = m.Run(ctx, channel.TCPClient(addr, channel.NoRetry)) }()
	defer func() { cancel(); <-done }()

	log.wait(t, secevent.KindAuthMasterErrorReceived)
	if _, ok := log.find(secevent.KindAuthMasterKeyChange); ok {
		t.Error("key change reported although the outstation refused it")
	}
}

func TestMasterReportsKeyChange(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 16)
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1,
		SecureAuthentication: &outstation.SecureAuthenticationConfig{Users: map[uint16][]byte{1: key}}},
		channel.TCPServer("127.0.0.1:0"))

	log := newEventLog()
	m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, Security: log.policy(),
		SecureAuthentication: &master.SecureAuthenticationConfig{User: 1, UpdateKey: key}}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = m.Run(ctx, channel.TCPClient(addr, channel.NoRetry)) }()
	defer func() { cancel(); <-done }()

	if e := log.wait(t, secevent.KindAuthMasterKeyChange); e.User != 1 {
		t.Errorf("event %+v", e)
	}
}
