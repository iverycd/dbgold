package security

import (
	"bytes"
	"sync"
	"testing"
)

func TestConcurrentSessionKeyNegotiation(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := NewClientKeyPair()
			if err != nil {
				t.Error(err)
				return
			}
			server, err := NewClientKeyPair()
			if err != nil {
				t.Error(err)
				return
			}
			a := ComputeSessionKey(client, server.y.Bytes())
			b := ComputeSessionKey(server, client.y.Bytes())
			if !bytes.Equal(a, b) {
				t.Error("independent sessions changed negotiated key")
			}
		}()
	}
	wg.Wait()
}
