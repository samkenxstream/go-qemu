// Copyright 2016 The go-qemu Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package qmp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSocketMonitorConnectDisconnect(t *testing.T) {
	_, _, done := testSocket(t)
	done()
}

func TestSocketMonitor_Listen(t *testing.T) {
	dir, err := ioutil.TempDir(os.TempDir(), "qemu-go.tests.")
	defer os.Remove(dir)
	if err != nil {
		t.Fatalf("failed to create temporary directory: %v", err)
	}
	sock := path.Join(dir, "sock")

	go func() {
		timer := time.NewTimer(50 * time.Millisecond)

		select {
		case <-timer.C:
			if _, err := os.Stat(sock); err == nil {
				_, err := net.Dial("unix", sock)
				if err != nil {
					t.Fatalf("failed to connect to socket %s: %v", sock, err)
				}
				break
			}
		case <-time.After(time.Second * 3):
			t.Fatalf("timed out connecting to socket %s", sock)
			break
		}
		timer.Stop()
	}()

	_, err = Listen("unix", sock)
	if err != nil {
		t.Fatalf("failed to listen with socket %s: %v", sock, err)
	}
}

func TestSocketMonitorEvents(t *testing.T) {
	mon, w, done := testSocket(t)
	defer done()

	events, err := mon.Events()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []Event{
		{Event: "STOP"},
		{Event: "SHUTDOWN"},
		{Event: "RESET"},
	}

	enc := json.NewEncoder(w)

	for i, e := range want {
		if err := enc.Encode(e); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		event := <-events
		t.Logf("[%02d] event: %q", i, event.Event)

		if want, got := e, event; !reflect.DeepEqual(want, got) {
			t.Fatalf("unexpected event:\n- want: %v\n-  got: %v",
				want, got)
		}
	}
}

func TestSocketMonitor_listenEmptyStream(t *testing.T) {
	mon := &SocketMonitor{listeners: new(int32)}

	r := strings.NewReader("")

	events := make(chan Event)
	stream := make(chan streamResponse)

	mon.listen(r, events, stream)

	if _, ok := <-events; ok {
		t.Fatal("events channel should be closed")
	}

	if _, ok := <-stream; ok {
		t.Fatal("stream channel should be closed")
	}
}

func TestSocketMonitor_listenScannerErr(t *testing.T) {
	mon := &SocketMonitor{listeners: new(int32)}

	errFoo := errors.New("foo")
	r := &errReader{err: errFoo}

	events := make(chan Event)
	stream := make(chan streamResponse)

	go mon.listen(r, events, stream)
	res := <-stream

	if want, got := errFoo, res.err; want != got {
		t.Fatalf("unexpected error:\n- want: %v\n-  got: %v", want, got)
	}
}

func TestSocketMonitor_listenInvalidJSON(t *testing.T) {
	mon := &SocketMonitor{listeners: new(int32)}

	r := strings.NewReader("<html>")

	events := make(chan Event)
	stream := make(chan streamResponse)

	mon.listen(r, events, stream)

	if _, ok := <-stream; ok {
		t.Fatal("stream channel should be closed")
	}
}

func TestSocketMonitor_listenStreamResponse(t *testing.T) {
	mon := &SocketMonitor{listeners: new(int32)}

	str := `{"foo": "bar"}`
	r := strings.NewReader(str)

	events := make(chan Event)
	stream := make(chan streamResponse)

	go mon.listen(r, events, stream)

	res := <-stream
	if res.err != nil {
		t.Fatalf("unexpected error: %v", res.err)
	}

	if want, got := str, string(res.buf); want != got {
		t.Fatalf("unexpected response:\n- want: %q\n-  got: %q", want, got)
	}
}

func TestSocketMonitor_listenEventNoListeners(t *testing.T) {
	mon := &SocketMonitor{listeners: new(int32)}

	r := strings.NewReader(`{"event":"STOP"}`)

	events := make(chan Event)
	stream := make(chan streamResponse)

	go mon.listen(r, events, stream)

	if _, ok := <-events; ok {
		t.Fatal("events channel should be closed")
	}
}

func TestSocketMonitor_listenEventOneListener(t *testing.T) {
	l := int32(1)
	mon := &SocketMonitor{listeners: &l}

	eventStop := "STOP"
	r := strings.NewReader(fmt.Sprintf(`{"event":%q}`, eventStop))

	events := make(chan Event)
	stream := make(chan streamResponse)

	go mon.listen(r, events, stream)

	e := <-events
	if want, got := eventStop, e.Event; want != got {
		t.Fatalf("unexpected event:\n- want: %q\n-  got: %q", want, got)
	}
}

func testSocket(t *testing.T) (*SocketMonitor, io.Writer, func()) {
	sc, tc := net.Pipe()

	mon := &SocketMonitor{
		c:         sc,
		listeners: new(int32),
	}

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		enc := json.NewEncoder(tc)
		dec := json.NewDecoder(tc)

		if err := enc.Encode(banner{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		var cmd Command
		if err := dec.Decode(&cmd); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if want, got := qmpCapabilities, cmd.Execute; want != got {
			t.Fatalf("unexpected capabilities handshake:\n- want: %q\n-  got: %q",
				want, got)
		}

		if err := enc.Encode(response{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}()

	if err := mon.Connect(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wg.Wait()

	return mon, tc, func() {
		if err := mon.Disconnect(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

type errReader struct {
	err error
}

func (r *errReader) Read(b []byte) (int, error) {
	return 0, r.err
}
