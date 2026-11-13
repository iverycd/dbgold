package dm

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestCancelInterruptsIOWithoutMutatingStatements(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	conn := &DmConnection{Access: &dm_build_2{dm_build_3: client}, closech: make(chan struct{}), stmtMap: make(map[int32]*DmStatement)}
	conn.startWatcher()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := conn.watchCancel(ctx); err != nil {
		t.Fatal(err)
	}
	// SQL execution owns these maps; cancellation must not traverse/free them.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := int32(0); i < 10000; i++ {
			conn.stmtMap[i] = &DmStatement{}
			delete(conn.stmtMap, i)
		}
	}()
	readDone := make(chan error, 1)
	go func() { _, err := client.Read(make([]byte, 1)); readDone <- err }()
	cancel()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("cancellation did not interrupt network I/O")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for SQL execution")
	}
	<-done
	conn.finish()
	if !conn.closed.IsSet() || !errors.Is(conn.canceled.Value(), context.Canceled) {
		t.Fatal("cancelled session could be reused")
	}
	if err := conn.close(); err != nil {
		t.Fatal(err)
	}
}
