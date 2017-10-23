// Copyright 2016-2017 Apcera Inc. All rights reserved.
package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/go-nats-streaming"
	"github.com/nats-io/go-nats-streaming/pb"
	"github.com/nats-io/nats-streaming-server/spb"
	"github.com/nats-io/nats-streaming-server/stores"
)

type mockedStore struct {
	stores.Store
}

type mockedMsgStore struct {
	stores.MsgStore
	sync.RWMutex
	fail bool
}

type mockedSubStore struct {
	stores.SubStore
	sync.RWMutex
	fail bool
}

func (ms *mockedStore) CreateChannel(name string) (*stores.Channel, error) {
	cs, err := ms.Store.CreateChannel(name)
	if err != nil {
		return nil, err
	}
	cs.Msgs = &mockedMsgStore{MsgStore: cs.Msgs}
	cs.Subs = &mockedSubStore{SubStore: cs.Subs}
	return cs, nil
}

func (ms *mockedMsgStore) Lookup(seq uint64) (*pb.MsgProto, error) {
	ms.RLock()
	fail := ms.fail
	ms.RUnlock()
	if fail {
		return nil, errOnPurpose
	}
	return ms.MsgStore.Lookup(seq)
}

func (ms *mockedMsgStore) FirstSequence() (uint64, error) {
	ms.RLock()
	fail := ms.fail
	ms.RUnlock()
	if fail {
		return 0, errOnPurpose
	}
	return ms.MsgStore.FirstSequence()
}

func (ms *mockedMsgStore) LastSequence() (uint64, error) {
	ms.RLock()
	fail := ms.fail
	ms.RUnlock()
	if fail {
		return 0, errOnPurpose
	}
	return ms.MsgStore.LastSequence()
}

func (ms *mockedMsgStore) FirstAndLastSequence() (uint64, uint64, error) {
	ms.RLock()
	fail := ms.fail
	ms.RUnlock()
	if fail {
		return 0, 0, errOnPurpose
	}
	return ms.MsgStore.FirstAndLastSequence()
}

func (ms *mockedMsgStore) GetSequenceFromTimestamp(startTime int64) (uint64, error) {
	ms.RLock()
	fail := ms.fail
	ms.RUnlock()
	if fail {
		return 0, errOnPurpose
	}
	return ms.MsgStore.GetSequenceFromTimestamp(startTime)
}

func TestStartPositionFailures(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	s.channels.Lock()
	s.channels.store = &mockedStore{Store: s.channels.store}
	s.channels.Unlock()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	if err := sc.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Unable to publish: %v", err)
	}

	cs := channelsGet(t, s.channels, "foo")
	mms := cs.store.Msgs.(*mockedMsgStore)
	mms.Lock()
	mms.fail = true
	mms.Unlock()

	queues := []string{"", "bar"}
	for _, q := range queues {
		// Take advantage of the QueueSubscribe() API that creates regular
		// subscription if second parameter is empty string.

		// New only
		if _, err := sc.QueueSubscribe("foo", q, func(_ *stan.Msg) {}); err == nil || !strings.Contains(err.Error(), errOnPurpose.Error()) {
			t.Fatalf("Not failed as expected: %v", err)
		}
		// Last received
		if _, err := sc.QueueSubscribe("foo", q, func(_ *stan.Msg) {}, stan.StartWithLastReceived()); err == nil || !strings.Contains(err.Error(), errOnPurpose.Error()) {
			t.Fatalf("Not failed as expected: %v", err)
		}
		// Time delta
		if _, err := sc.QueueSubscribe("foo", q, func(_ *stan.Msg) {}, stan.StartAtTimeDelta(time.Second)); err == nil || !strings.Contains(err.Error(), errOnPurpose.Error()) {
			t.Fatalf("Not failed as expected: %v", err)
		}
		// Sequence start
		if _, err := sc.QueueSubscribe("foo", q, func(_ *stan.Msg) {}, stan.StartAtSequence(1)); err == nil || !strings.Contains(err.Error(), errOnPurpose.Error()) {
			t.Fatalf("Not failed as expected: %v", err)
		}
		// First
		if _, err := sc.QueueSubscribe("foo", q, func(_ *stan.Msg) {}, stan.StartAt(pb.StartPosition_First)); err == nil || !strings.Contains(err.Error(), errOnPurpose.Error()) {
			t.Fatalf("Not failed as expected: %v", err)
		}
	}
}

type checkErrorLogger struct {
	dummyLogger
	checkErrorStr string
	gotError      bool
}

func (l *checkErrorLogger) Errorf(format string, args ...interface{}) {
	l.log(format, args...)
	l.Lock()
	if strings.Contains(l.msg, l.checkErrorStr) {
		l.gotError = true
	}
	l.Unlock()
}

func TestMsgLookupFailures(t *testing.T) {
	logger := &checkErrorLogger{checkErrorStr: "looking up"}
	opts := GetDefaultOptions()
	opts.CustomLogger = logger
	s, err := RunServerWithOpts(opts, nil)
	if err != nil {
		t.Fatalf("Error running server: %v", err)
	}
	defer s.Shutdown()

	s.channels.Lock()
	s.channels.store = &mockedStore{Store: s.channels.store}
	s.channels.Unlock()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	rcvCh := make(chan bool)
	sub, err := sc.Subscribe("foo", func(_ *stan.Msg) {
		rcvCh <- true
	})
	if err != nil {
		t.Fatalf("Error on subscribe: %v", err)
	}

	cs := channelsGet(t, s.channels, "foo")
	mms := cs.store.Msgs.(*mockedMsgStore)
	mms.Lock()
	mms.fail = true
	mms.Unlock()

	if err := sc.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Unable to publish: %v", err)
	}

	select {
	case <-rcvCh:
		t.Fatal("Should not have received the message")
	case <-time.After(100 * time.Millisecond):
		// we waited "long enoug" and did not receive anything, which is good
	}
	logger.Lock()
	gotErr := logger.gotError
	logger.Unlock()
	if !gotErr {
		t.Fatalf("Did not capture error about lookup")
	}
	mms.Lock()
	mms.fail = false
	mms.Unlock()
	sub.Unsubscribe()

	// Create subscription, manual ack mode, don't ack, wait for redelivery
	sub, err = sc.Subscribe("foo", func(_ *stan.Msg) {
		rcvCh <- true
	}, stan.DeliverAllAvailable(), stan.SetManualAckMode(), stan.AckWait(ackWaitInMs(15)))
	if err != nil {
		t.Fatalf("Error on subscribe: %v", err)
	}
	if err := Wait(rcvCh); err != nil {
		t.Fatal("Did not get our message")
	}
	// Activate store failure
	mms.Lock()
	mms.fail = true
	logger.Lock()
	logger.checkErrorStr = "Error getting message for redelivery"
	logger.gotError = false
	logger.Unlock()
	mms.Unlock()
	// Make sure message is not redelivered and we capture the error
	select {
	case <-rcvCh:
		t.Fatal("Should not have received the message")
	case <-time.After(100 * time.Millisecond):
		// we waited more than redelivery time and did not receive anything, which is good
	}
	logger.Lock()
	gotErr = logger.gotError
	logger.Unlock()
	if !gotErr {
		t.Fatalf("Did not capture error about redelivery")
	}
	mms.Lock()
	mms.fail = false
	mms.Unlock()
	sub.Unsubscribe()
}

func (ss *mockedSubStore) CreateSub(sub *spb.SubState) error {
	ss.RLock()
	fail := ss.fail
	ss.RUnlock()
	if fail {
		return fmt.Errorf("On purpose")
	}
	return ss.SubStore.CreateSub(sub)
}

func (ss *mockedSubStore) AddSeqPending(subid, seq uint64) error {
	ss.RLock()
	fail := ss.fail
	ss.RUnlock()
	if fail {
		return fmt.Errorf("On purpose")
	}
	return ss.SubStore.AddSeqPending(subid, seq)
}

func (ss *mockedSubStore) UpdateSub(sub *spb.SubState) error {
	ss.RLock()
	fail := ss.fail
	ss.RUnlock()
	if fail {
		return fmt.Errorf("On purpose")
	}
	return ss.SubStore.UpdateSub(sub)
}

func (ss *mockedSubStore) DeleteSub(subid uint64) error {
	ss.RLock()
	fail := ss.fail
	ss.RUnlock()
	if fail {
		return fmt.Errorf("On purpose")
	}
	return ss.SubStore.DeleteSub(subid)
}

func TestCreateSubFailures(t *testing.T) {
	logger := &checkErrorLogger{checkErrorStr: "store subscription"}
	opts := GetDefaultOptions()
	opts.CustomLogger = logger
	s, err := RunServerWithOpts(opts, nil)
	if err != nil {
		t.Fatalf("Error running server: %v", err)
	}
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	waitForNumClients(t, s, 1)

	s.channels.Lock()
	s.channels.store = &mockedStore{Store: s.channels.store}
	s.channels.Unlock()

	c := channelsLookupOrCreate(t, s, "foo")
	mss := c.store.Subs.(*mockedSubStore)
	mss.Lock()
	mss.fail = true
	mss.Unlock()

	// Check that server reported an error
	checkError := func() {
		logger.Lock()
		gotIt := logger.gotError
		logger.gotError = false
		logger.Unlock()
		if !gotIt {
			stackFatalf(t, "Server did not log expected error")
		}
	}

	// Create a plain sub
	if _, err := sc.Subscribe("foo", func(_ *stan.Msg) {}); err == nil {
		t.Fatal("Expected subscribe to fail")
	}
	checkError()

	// Create a durable
	if _, err := sc.Subscribe("foo", func(_ *stan.Msg) {}, stan.DurableName("dur")); err == nil {
		t.Fatal("Expected subscribe to fail")
	}
	checkError()

	// Create a queue sub
	if _, err := sc.QueueSubscribe("foo", "queue", func(_ *stan.Msg) {}); err == nil {
		t.Fatal("Expected subscribe to fail")
	}
	checkError()

	// Create a durable queue sub with manual ack and does not ack message
	if _, err := sc.QueueSubscribe("foo", "dqueue", func(_ *stan.Msg) {}, stan.DurableName("dur")); err == nil {
		t.Fatal("Expected subscribe to fail")
	}
	checkError()
}

func TestDeleteSubFailures(t *testing.T) {
	logger := &checkErrorLogger{checkErrorStr: "deleting subscription"}
	opts := GetDefaultOptions()
	opts.CustomLogger = logger
	s, err := RunServerWithOpts(opts, nil)
	if err != nil {
		t.Fatalf("Error running server: %v", err)
	}
	defer s.Shutdown()

	s.channels.Lock()
	s.channels.store = &mockedStore{Store: s.channels.store}
	s.channels.Unlock()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	// Create a plain sub
	psub, err := sc.Subscribe("foo", func(_ *stan.Msg) {})
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Create a queue sub
	qsub, err := sc.QueueSubscribe("foo", "queue", func(_ *stan.Msg) {})
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Create a durable queue sub with manual ack and does not ack message
	ch := make(chan bool)
	dqsub1, err := sc.QueueSubscribe("foo", "dqueue", func(_ *stan.Msg) {
		ch <- true
	}, stan.DurableName("dur"), stan.SetManualAckMode())
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Produce a message to this durable queue sub
	if err := sc.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Error on publish: %v", err)
	}
	// Create 2 more durable queue subs
	dqsub2, err := sc.QueueSubscribe("foo", "dqueue", func(_ *stan.Msg) {},
		stan.DurableName("dur"))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	if _, err := sc.QueueSubscribe("foo", "dqueue", func(_ *stan.Msg) {},
		stan.DurableName("dur")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}

	// Ensure subscription is processed
	waitForNumSubs(t, s, clientName, 5)

	cs := channelsGet(t, s.channels, "foo")
	mss := cs.store.Subs.(*mockedSubStore)
	mss.Lock()
	mss.fail = true
	mss.Unlock()

	// Check that server reported an error
	checkError := func() {
		logger.Lock()
		gotIt := logger.gotError
		logger.gotError = false
		logger.Unlock()
		if !gotIt {
			stackFatalf(t, "Server did not log error on unsubscribe")
		}
	}

	// Now unsubscribe
	if err := psub.Unsubscribe(); err != nil {
		t.Fatalf("Unexpected error on unsubscribe: %v", err)
	}
	// Wait for unsubscribe to be processed
	waitForNumSubs(t, s, clientName, 4)
	checkError()

	// Unsubscribe queue sub
	if err := qsub.Unsubscribe(); err != nil {
		t.Fatalf("Unexpected error on unsubscribe: %v", err)
	}
	// Wait for unsubscribe to be processed
	waitForNumSubs(t, s, clientName, 3)
	checkError()

	// Close 1 durable queue sub
	if err := dqsub2.Close(); err != nil {
		t.Fatalf("Error on close: %v", err)
	}
	// Wait for close to be processed
	waitForNumSubs(t, s, clientName, 2)
	checkError()

	// Unsubscribe last durable queue member.
	if err := dqsub1.Unsubscribe(); err != nil {
		t.Fatalf("Error on close: %v", err)
	}
	// Wait for close to be processed
	waitForNumSubs(t, s, clientName, 1)
	checkError()
}

func TestUpdateSubFailure(t *testing.T) {
	logger := &checkErrorLogger{checkErrorStr: "add subscription"}
	opts := GetDefaultOptions()
	opts.CustomLogger = logger
	s, err := RunServerWithOpts(opts, nil)
	if err != nil {
		t.Fatalf("Error running server: %v", err)
	}
	defer s.Shutdown()

	s.channels.Lock()
	s.channels.store = &mockedStore{Store: s.channels.store}
	s.channels.Unlock()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	dur, err := sc.Subscribe("foo", func(_ *stan.Msg) {}, stan.DurableName("dur"))
	if err != nil {
		t.Fatalf("Error on subscribe: %v", err)
	}
	waitForNumSubs(t, s, clientName, 1)
	dur.Close()
	waitForNumSubs(t, s, clientName, 0)

	cs := channelsGet(t, s.channels, "foo")
	mss := cs.store.Subs.(*mockedSubStore)
	mss.Lock()
	mss.fail = true
	mss.Unlock()
	if _, err := sc.Subscribe("foo", func(_ *stan.Msg) {}, stan.DurableName("dur")); err == nil {
		t.Fatal("Expected subscription to fail")
	}
	logger.Lock()
	gotIt := logger.gotError
	logger.Unlock()
	if !gotIt {
		t.Fatalf("Server did not log error on subscribe")
	}
}

func TestSendMsgToSubStoreFailure(t *testing.T) {
	logger := &checkErrorLogger{checkErrorStr: "add pending message"}
	opts := GetDefaultOptions()
	opts.CustomLogger = logger
	s, err := RunServerWithOpts(opts, nil)
	if err != nil {
		t.Fatalf("Error running server: %v", err)
	}
	defer s.Shutdown()

	s.channels.Lock()
	s.channels.store = &mockedStore{Store: s.channels.store}
	s.channels.Unlock()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	if _, err := sc.Subscribe("foo", func(_ *stan.Msg) {}); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	waitForNumSubs(t, s, clientName, 1)

	// Cause failure on AddSeqPending
	cs := channelsGet(t, s.channels, "foo")
	mss := cs.store.Subs.(*mockedSubStore)
	mss.Lock()
	mss.fail = true
	mss.Unlock()
	if err := sc.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Check error was logged.
	logger.Lock()
	gotIt := logger.gotError
	logger.Unlock()
	if !gotIt {
		t.Fatal("Server did not log error about updating subscription")
	}
}

func TestClientStoreError(t *testing.T) {
	logger := &checkErrorLogger{checkErrorStr: "deleting client"}
	opts := GetDefaultOptions()
	opts.CustomLogger = logger
	s, err := RunServerWithOpts(opts, nil)
	if err != nil {
		t.Fatalf("Error running server: %v", err)
	}
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	s.clients.Lock()
	s.clients.store = &clientStoreErrorsStore{Store: s.clients.store}
	s.clients.Unlock()

	// Client should not fail to close
	if err := sc.Close(); err != nil {
		t.Fatalf("Error on close: %v", err)
	}
	// However, server should have logged something about an error closing client
	logger.Lock()
	gotIt := logger.gotError
	logger.Unlock()
	if !gotIt {
		t.Fatal("Server did not report error about closing client")
	}
	// Verify that client is gone though
	if c := s.clients.lookup(clientName); c != nil {
		t.Fatalf("Unexpected client in server: %v", c)
	}

	logger.Lock()
	logger.gotError = false
	logger.checkErrorStr = "registering client"
	logger.Unlock()

	if _, err := stan.Connect(clusterName, clientName); err == nil || !strings.Contains(err.Error(), errOnPurpose.Error()) {
		t.Fatalf("Expected error on connect, got %v", err)
	}
	logger.Lock()
	gotIt = logger.gotError
	logger.Unlock()
	if !gotIt {
		t.Fatal("Server did not report error about registering client")
	}
	if c := s.clients.lookup(clientName); c != nil {
		t.Fatalf("Unexpected client in server: %v", c)
	}
}
