//go:build !linux

package qemu

import (
	"errors"
	"os"
)

// fdStore is systemd's; elsewhere VMs keep path (or loopback) QMP sockets.
type fdStore struct{}

func openFDStore() *fdStore                   { return nil }
func (s *fdStore) put(string, *os.File) error { return errors.New("no fd store") }
func (s *fdStore) remove(string) error        { return nil }
func (s *fdStore) take(string) *os.File       { return nil }
func socketPair() (*os.File, *os.File, error) { return nil, nil, errors.New("no socketpair") }
