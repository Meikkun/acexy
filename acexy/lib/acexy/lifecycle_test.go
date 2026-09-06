package acexy

import (
	"encoding/json"
	"io"
	"sync"
	"testing"
)

func TestEvictionAndDisconnectRemoveClientOnce(t *testing.T) {
	for _, evictionFirst := range []bool{false, true} {
		t.Run(map[bool]string{true: "eviction first", false: "disconnect first"}[evictionFirst], func(t *testing.T) {
			a := newAdmissionBackend(t, &admissionBackend{})
			stream, err := a.FetchStream(AceID{id: "one"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			first, second := &discardClient{}, &discardClient{}
			for _, w := range []io.Writer{first, second} {
				if err := a.StartStream(stream, w); err != nil {
					t.Fatal(err)
				}
			}
			a.mutex.Lock()
			session := a.streams[stream.ID]
			registration := session.registrations[second]
			a.mutex.Unlock()
			callbackReached, allowCallback, callbackDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				close(callbackReached)
				<-allowCallback
				a.removeClient(session, registration, "late eviction")
				close(callbackDone)
			}()
			<-callbackReached
			if evictionFirst {
				close(allowCallback)
				<-callbackDone
				a.StopStream(stream, second)
			} else {
				a.StopStream(stream, second)
				close(allowCallback)
				<-callbackDone
			}
			if got := a.ClientCount(); got != 1 {
				t.Fatalf("duplicate cleanup removed healthy viewer: %d", got)
			}
			before := first.bytes.Load()
			eventually(t, func() bool { return first.bytes.Load() > before })
		})
	}
}

func TestOldRegistrationCallbackCannotRemoveReattachedWriter(t *testing.T) {
	a := newAdmissionBackend(t, &admissionBackend{})
	stream, err := a.FetchStream(AceID{id: "reattach"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	anchor, reused := &discardClient{}, &discardClient{}
	for _, w := range []io.Writer{anchor, reused} {
		if err := a.StartStream(stream, w); err != nil {
			t.Fatal(err)
		}
	}
	a.mutex.Lock()
	session := a.streams[stream.ID]
	oldRegistration := session.registrations[reused]
	a.mutex.Unlock()
	a.StopStream(stream, reused)
	if err := a.StartStream(stream, reused); err != nil {
		t.Fatal(err)
	}
	a.removeClient(session, oldRegistration, "delayed callback")
	if a.ClientCount() != 2 {
		t.Fatal("old registration removed reattached writer")
	}
	before := reused.bytes.Load()
	eventually(t, func() bool { return reused.bytes.Load() > before })
}

func TestStatusSnapshotsConcurrentAttachDetach(t *testing.T) {
	a := newAdmissionBackend(t, &admissionBackend{})
	stream, err := a.FetchStream(AceID{id: "status"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.StartStream(stream, &discardClient{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			w := &discardClient{}
			if err := a.StartStream(stream, w); err != nil {
				t.Error(err)
				return
			}
			a.StopStream(stream, w)
		}
	}()
	for i := 0; i < 100; i++ {
		status, err := a.GetStatus(&stream.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := json.Marshal(status); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestStatusCopiesClientCounts(t *testing.T) {
	a := &Acexy{}
	a.Init()
	id := AceID{id: "one"}
	w := &mockWriter{}
	a.streams[id] = &ongoingStream{
		stream:        &AceStream{ID: id},
		registrations: map[io.Writer]*clientRegistration{w: {}},
	}
	status, err := a.GetStatus(&id)
	if err != nil {
		t.Fatal(err)
	}
	delete(a.streams[id].registrations, w)
	if *status.Clients != 1 {
		t.Fatal("status aliases live client count")
	}
}
