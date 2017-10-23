// Copyright 2016-2017 Apcera Inc. All rights reserved.
package server

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/go-nats"
	"github.com/nats-io/go-nats-streaming"
	"github.com/nats-io/go-nats-streaming/pb"
	"github.com/nats-io/nats-streaming-server/stores"
)

// AckWait is for the whole group. The first member that
// creates the group dictates what the AckWait will be.
func TestQueueSubsWithDifferentAckWait(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	var qsub1, qsub2, qsub3 stan.Subscription
	var err error

	dch := make(chan bool)
	rch2 := make(chan bool)
	rch3 := make(chan bool)

	cb := func(m *stan.Msg) {
		if !m.Redelivered {
			dch <- true
		} else {
			if m.Sub == qsub2 {
				rch2 <- true
			} else if m.Sub == qsub3 {
				rch3 <- true
				// stop further redeliveries, test is done.
				qsub3.Close()
			}
		}
	}
	// Create first queue member with high AckWait
	qsub1, err = sc.QueueSubscribe("foo", "bar", cb,
		stan.SetManualAckMode(),
		stan.AckWait(ackWaitInMs(200)))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Send the single message used in this test
	if err := sc.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Wait for message to be received
	if err := Wait(dch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Create the second member with low AckWait
	qsub2, err = sc.QueueSubscribe("foo", "bar", cb,
		stan.SetManualAckMode(),
		stan.AckWait(ackWaitInMs(15)))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Check we have the two members
	checkQueueGroupSize(t, s, "foo", "bar", true, 2)
	// Close the first, message should be redelivered within
	// qsub1's AckWait, which is 200ms.
	qsub1.Close()
	// Check we have only 1 member
	checkQueueGroupSize(t, s, "foo", "bar", true, 1)
	// Wait for redelivery
	select {
	case <-rch2:
	// ok
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Message should have been redelivered")
	}
	// Create 3rd member with higher AckWait than the 2nd
	qsub3, err = sc.QueueSubscribe("foo", "bar", cb,
		stan.SetManualAckMode(),
		stan.AckWait(ackWaitInMs(350)))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Close qsub2
	qsub2.Close()
	// Check we have only 1 member
	checkQueueGroupSize(t, s, "foo", "bar", true, 1)
	// Wait for redelivery. It should happen after the remaining
	// of the first redelivery to qsub2 and the AckWait, which
	// should be less than 200 ms.
	select {
	case <-rch3:
	// ok
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Message should have been redelivered")
	}
}

func TestQueueMaxInFlight(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	total := 100
	payload := []byte("hello")
	for i := 0; i < total; i++ {
		sc.Publish("foo", payload)
	}

	ch := make(chan bool)
	received := 0
	cb := func(m *stan.Msg) {
		if !m.Redelivered {
			received++
			if received == total {
				ch <- true
			}
		}
	}
	if _, err := sc.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.MaxInflight(5)); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}

	if err := Wait(ch); err != nil {
		t.Fatal("Did not get all our messages")
	}
}

func TestQueueGroupRemovedOnLastMemberLeaving(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	if err := sc.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan bool)
	cb := func(m *stan.Msg) {
		if m.Sequence == 1 {
			ch <- true
		}
	}
	// Create a queue subscriber
	if _, err := sc.QueueSubscribe("foo", "group", cb, stan.DeliverAllAvailable()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Close the connection which will remove the queue subscriber
	sc.Close()

	// Create a new connection
	sc = NewDefaultConnection(t)
	defer sc.Close()
	// Send a new message
	if err := sc.Publish("foo", []byte("msg2")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Start a queue subscriber. The group should have been destroyed
	// when the last member left, so even with a new name, this should
	// be a new group and start from msg seq 1
	qsub, err := sc.QueueSubscribe("foo", "group", cb, stan.DeliverAllAvailable())
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Test with unsubscribe
	if err := qsub.Unsubscribe(); err != nil {
		t.Fatalf("Error during Unsubscribe: %v", err)
	}
	// Recreate a queue subscriber, it should again receive from msg1
	if _, err := sc.QueueSubscribe("foo", "group", cb, stan.DeliverAllAvailable()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
}

func TestQueueSubscriberMsgsRedeliveredOnClose(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	if err := sc.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	var sub1 stan.Subscription
	var sub2 stan.Subscription
	var err error
	ch := make(chan bool)
	qsetup := make(chan bool)
	cb := func(m *stan.Msg) {
		<-qsetup
		if m.Sub == sub1 && m.Sequence == 1 && !m.Redelivered {
			ch <- true
		} else if m.Sub == sub2 && m.Sequence == 1 && m.Redelivered {
			ch <- true
		}
	}
	// Create a queue subscriber with MaxInflight == 1 and manual ACK
	// so that it does not ack it and see if it will be redelivered.
	sub1, err = sc.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.MaxInflight(1),
		stan.SetManualAckMode(),
		stan.AckWait(ackWaitInMs(250)))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	qsetup <- true
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Start 2nd queue subscriber on same group
	sub2, err = sc.QueueSubscribe("foo", "group", cb, stan.AckWait(ackWaitInMs(15)))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Unsubscribe the first member
	sub1.Unsubscribe()
	qsetup <- true
	// The second queue subscriber should receive the first message.
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
}

func TestPersistentStoreQueueSubLeavingUpdateQGroupLastSent(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)

	opts := getTestDefaultOptsForPersistentStore()
	s := runServerWithOpts(t, opts, nil)
	defer shutdownRestartedServerOnTestExit(&s)

	sc, nc := createConnectionWithNatsOpts(t, clientName, nats.ReconnectWait(50*time.Millisecond))
	defer nc.Close()
	defer sc.Close()

	if err := sc.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan *stan.Msg)
	cb := func(m *stan.Msg) {
		ch <- m
	}
	// First queue sub should receive the message that we just sent
	if _, err := sc.QueueSubscribe("foo", "group", cb, stan.DeliverAllAvailable()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	select {
	case <-ch:
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("Should have received first message")
	}
	// Start 2nd queue subscriber on same group
	sub2, err := sc.QueueSubscribe("foo", "group", cb)
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// send second message
	if err := sc.Publish("foo", []byte("msg2")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// The second queue subscriber should receive the second message.
	select {
	case <-ch:
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("Should have received second message")
	}
	// Unsubscribe the second member
	sub2.Unsubscribe()
	// Wait for the unsubscribe to be processed
	waitForNumSubs(t, s, clientName, 1)
	// Restart server
	s.Shutdown()
	s = runServerWithOpts(t, opts, nil)
	// Send a third message
	if err := sc.Publish("foo", []byte("msg3")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	select {
	case m3 := <-ch:
		if string(m3.Data) != "msg3" {
			t.Fatalf("Unexpected third message: %v", string(m3.Data))
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatalf("Should have received third message")
	}
}

func checkQueueGroupSize(t *testing.T, s *StanServer, channelName, groupName string, expectedExist bool, expectedSize int) {
	cs := channelsGet(t, s.channels, channelName)
	s.mu.RLock()
	groupSize := 0
	group, exist := cs.ss.qsubs[groupName]
	if exist {
		groupSize = len(group.members)
	}
	s.mu.RUnlock()
	if expectedExist && !exist {
		stackFatalf(t, "Expected group to still exist, does not")
	}
	if !expectedExist && exist {
		stackFatalf(t, "Expected group to not exist, it does")
	}
	if expectedExist {
		if groupSize != expectedSize {
			stackFatalf(t, "Expected group size to be %v, got %v", expectedSize, groupSize)
		}
	}
}

func TestBasicDurableQueueSub(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc1 := NewDefaultConnection(t)
	defer sc1.Close()
	if err := sc1.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan bool)
	count := 0
	cb := func(m *stan.Msg) {
		count++
		if count == 1 && m.Sequence == 1 {
			ch <- true
		} else if count == 2 && m.Sequence == 2 {
			ch <- true
		}
	}
	// Create a durable queue subscriber.
	sc2, err := stan.Connect(clusterName, "sc2cid")
	if err != nil {
		stackFatalf(t, "Expected to connect correctly, got err %v", err)
	}
	defer sc2.Close()
	if _, err := sc2.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Check group exists
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 1)
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// For this test, make sure we wait for ack to be processed.
	waitForQGroupAcks(t, s, "foo", "qsub:group", 0)
	// Close the durable queue sub's connection.
	// This should not close the queue group
	sc2.Close()
	// Check queue group still exist, but size 0
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 0)
	// Send another message
	if err := sc1.Publish("foo", []byte("msg2")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Create a durable queue subscriber on the group.
	sub, err := sc1.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub"))
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// It should receive message 2 only
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Now unsubscribe the sole member
	if err := sub.Unsubscribe(); err != nil {
		t.Fatalf("Error during Unsubscribe: %v", err)
	}
	// Group should be gone.
	checkQueueGroupSize(t, s, "foo", "qsub:group", false, 0)
}

func TestDurableAndNonDurableQueueSub(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()
	// Expect failure if durable name contains ':'
	_, err := sc.QueueSubscribe("foo", "group", func(_ *stan.Msg) {},
		stan.DurableName("qsub:"))
	if err == nil {
		t.Fatal("Expected error on subscribe")
	}
	if err.Error() != ErrInvalidDurName.Error() {
		t.Fatalf("Expected error %v, got %v", ErrInvalidDurName, err)
	}

	if err := sc.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan bool)
	cb := func(m *stan.Msg) {
		ch <- true
	}
	// Create a durable queue subscriber.
	if _, err := sc.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Create a regular one
	if _, err := sc.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Check groups exists
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 1)
	checkQueueGroupSize(t, s, "foo", "group", true, 1)
	// Wait to receive the messages
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Close connection
	sc.Close()
	// Non durable group should be gone
	checkQueueGroupSize(t, s, "foo", "group", false, 0)
	// Other should exist, but empty
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 0)
}

func TestPersistentStoreDurableQueueSub(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)

	opts := getTestDefaultOptsForPersistentStore()
	s := runServerWithOpts(t, opts, nil)
	defer shutdownRestartedServerOnTestExit(&s)

	sc1, nc1 := createConnectionWithNatsOpts(t, clientName, nats.ReconnectWait(50*time.Millisecond))
	defer nc1.Close()
	defer sc1.Close()
	if err := sc1.Publish("foo", []byte("msg1")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	ch := make(chan bool)
	count := 0
	cb := func(m *stan.Msg) {
		count++
		if count == 1 && m.Sequence == 1 {
			ch <- true
		} else if count == 2 && m.Sequence == 2 {
			ch <- true
		}
	}
	// Create a durable queue subscriber.
	sc2, err := stan.Connect(clusterName, "sc2cid")
	if err != nil {
		stackFatalf(t, "Expected to connect correctly, got err %v", err)
	}
	defer sc2.Close()
	if _, err := sc2.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Wait to receive the message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// For this test, make sure ack is processed
	waitForQGroupAcks(t, s, "foo", "qsub:group", 0)
	// Close the durable queue sub's connection.
	// This should not close the queue group
	sc2.Close()
	// Check queue group still exist, but size 0
	checkQueueGroupSize(t, s, "foo", "qsub:group", true, 0)
	// Send another message
	if err := sc1.Publish("foo", []byte("msg2")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Stop and restart the server
	s.Shutdown()
	s = runServerWithOpts(t, opts, nil)
	// Create another client connection
	sc2, err = stan.Connect(clusterName, "sc2cid")
	if err != nil {
		stackFatalf(t, "Expected to connect correctly, got err %v", err)
	}
	defer sc2.Close()
	// Create a durable queue subscriber on the group.
	if _, err := sc2.QueueSubscribe("foo", "group", cb,
		stan.DeliverAllAvailable(),
		stan.DurableName("qsub")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// It should receive message 2 only
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
}

func TestPersistentStoreQMemberRemovedFromStore(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)

	opts := getTestDefaultOptsForPersistentStore()
	s := runServerWithOpts(t, opts, nil)
	defer shutdownRestartedServerOnTestExit(&s)

	sc1 := NewDefaultConnection(t)
	defer sc1.Close()

	ch := make(chan bool)
	// Create the group (adding the first member)
	if _, err := sc1.QueueSubscribe("foo", "group",
		func(_ *stan.Msg) {
			ch <- true
		},
		stan.DurableName("dur")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}

	sc2, err := stan.Connect(clusterName, "othername")
	if err != nil {
		stackFatalf(t, "Expected to connect correctly, got err %v", err)
	}
	defer sc2.Close()
	// Add a second member to the group
	if _, err := sc2.QueueSubscribe("foo", "group", func(_ *stan.Msg) {},
		stan.DurableName("dur")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Remove it by closing the connection.
	sc2.Close()

	// Send a message and verify it is received
	if err := sc1.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Have the first leave the group too.
	sc1.Close()

	// Restart the server
	s.Shutdown()
	s = runServerWithOpts(t, opts, nil)

	sc := NewDefaultConnection(t)
	defer sc.Close()

	// Send a new message
	if err := sc.Publish("foo", []byte("hello")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Have a member rejoin the group
	if _, err := sc.QueueSubscribe("foo", "group",
		func(_ *stan.Msg) {
			ch <- true
		},
		stan.DurableName("dur")); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// It should receive the second message
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Check server state
	s.mu.RLock()
	cs := channelsLookupOrCreate(t, s, "foo")
	ss := cs.ss
	s.mu.RUnlock()
	ss.RLock()
	qs := ss.qsubs["dur:group"]
	ss.RUnlock()
	if len(qs.members) != 1 {
		t.Fatalf("Expected only 1 member, got %v", len(qs.members))
	}
}

func TestQueueMaxInflightAppliesToGroup(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	ch := make(chan bool, 1)
	// Create a member with low MaxInflight and Manual ack mode
	if _, err := sc.QueueSubscribe("foo", "queue",
		func(m *stan.Msg) {
			if m.Sequence == 1 {
				ch <- true
			}
		},
		stan.MaxInflight(1),
		stan.AckWait(ackWaitInMs(50)),
		stan.SetManualAckMode()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := sc.Publish("foo", []byte("msg")); err != nil {
			t.Fatalf("Unexpected error on publish: %v", err)
		}
	}
	// Check message is received
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	// Create another member with higher MaxInFlight but this should
	// be ignored and it should not receive the second message,
	// but it should receive the first as a redelivered.
	count := 0
	errCh := make(chan error, 1)
	if _, err := sc.QueueSubscribe("foo", "queue",
		func(m *stan.Msg) {
			count++
			if count == 1 {
				if m.Sequence != 1 {
					errCh <- fmt.Errorf("received %v as the first message", m.Sequence)
				} else {
					errCh <- nil
				}
			}
		},
		stan.MaxInflight(10),
		stan.SetManualAckMode()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	select {
	case e := <-errCh:
		if e != nil {
			t.Fatalf(e.Error())
		}
	case <-time.After(time.Second):
		t.Fatalf("Message should have been redelivered")
	}
}

type queueGroupStalledMsgStore struct {
	stores.MsgStore
	lookupCh chan struct{}
}

func (s *queueGroupStalledMsgStore) Lookup(seq uint64) (*pb.MsgProto, error) {
	s.lookupCh <- struct{}{}
	return s.MsgStore.Lookup(seq)
}

func TestQueueGroupStalledSemantics(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)
	opts := getTestDefaultOptsForPersistentStore()
	s := runServerWithOpts(t, opts, nil)
	defer shutdownRestartedServerOnTestExit(&s)

	sc, nc := createConnectionWithNatsOpts(t, clientName, nats.ReconnectWait(50*time.Millisecond))
	defer nc.Close()
	defer sc.Close()

	ch := make(chan bool)
	cb := func(_ *stan.Msg) {
		ch <- true
	}
	// Create a member with manual ack and MaxInFlight of 2
	if _, err := sc.QueueSubscribe("foo", "queue", cb,
		stan.SetManualAckMode(), stan.MaxInflight(2)); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	if err := sc.Publish("foo", []byte("msg")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	checkStalled := func(expected bool) {
		var stalled bool
		timeout := time.Now().Add(time.Second)
		for time.Now().Before(timeout) {
			c := channelsGet(t, s.channels, "foo")
			c.ss.RLock()
			qs := c.ss.qsubs["queue"]
			c.ss.RUnlock()
			qs.RLock()
			qs.sub.RLock()
			stalled = qs.sub.stalled
			qs.sub.RUnlock()
			qs.RUnlock()
			if stalled != expected {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			break
		}
		if stalled != expected {
			stackFatalf(t, "Expected stalled to be %v, got %v", expected, stalled)
		}
	}
	checkStalled(false)

	// Create another member
	if _, err := sc.QueueSubscribe("foo", "queue", cb,
		stan.SetManualAckMode()); err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}
	// Publish 2nd message
	if err := sc.Publish("foo", []byte("msg")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Wait for message to received
	if err := Wait(ch); err != nil {
		t.Fatal("Did not get our message")
	}
	checkStalled(true)
	waitForQGroupAcks(t, s, "foo", "queue", 2)

	// Replace store with one that will report if Lookup was used
	s.channels.Lock()
	c := s.channels.channels["foo"]
	ms := &queueGroupStalledMsgStore{c.store.Msgs, make(chan struct{}, 2)}
	c.store.Msgs = ms
	s.channels.Unlock()
	// Publish a message
	if err := sc.Publish("foo", []byte("msg")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	// Check that no Lookup was done
	select {
	case <-ms.lookupCh:
		t.Fatalf("Lookup should not have been invoked")
	case <-time.After(50 * time.Millisecond):
	}

	// Restart server...
	s.Shutdown()
	s = runServerWithOpts(t, opts, nil)
	// Serve will check if messages need to be redelivered, but since it
	// is less than the AckWait of 30 seconds, it won't send them. Still,
	// they are looked up at this point. So wait a little before swapping
	// with mock store.
	time.Sleep(100 * time.Millisecond)

	// Replace store with one that will report if Lookup was used
	s.channels.Lock()
	c = s.channels.channels["foo"]
	orgMS := c.store.Msgs
	ms = &queueGroupStalledMsgStore{c.store.Msgs, make(chan struct{}, 2)}
	c.store.Msgs = ms
	s.channels.Unlock()

	// Publish a message
	if err := sc.Publish("foo", []byte("msg")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}
	select {
	case <-ms.lookupCh:
		t.Fatalf("Lookup should not have been invoked")
	case <-time.After(50 * time.Millisecond):
	}

	// Then replace store with original one before exit.
	s.channels.Lock()
	c = s.channels.channels["foo"]
	c.store.Msgs = orgMS
	s.channels.Unlock()
}

func TestQueueGroupUnStalledOnAcks(t *testing.T) {
	s := runServer(t, clusterName)
	defer s.Shutdown()

	sc := NewDefaultConnection(t)
	defer sc.Close()

	// Produce messages to a channel
	msgCount := 10000
	msg := []byte("hello")
	for i := 0; i < msgCount; i++ {
		if i == msgCount-1 {
			if err := sc.Publish("foo", msg); err != nil {
				t.Fatalf("Error on publish: %v", err)
			}
		} else {
			if _, err := sc.PublishAsync("foo", msg, func(guid string, puberr error) {
				if puberr != nil {
					t.Fatalf("Error on publish %q: %v", guid, puberr)
				}
			}); err != nil {
				t.Fatalf("Error on publish: %v", err)
			}
		}
	}

	count := int32(0)
	doneCh := make(chan bool)
	mcb := func(m *stan.Msg) {
		if c := atomic.AddInt32(&count, 1); c == int32(msgCount) {
			doneCh <- true
		}
	}

	// Create multiple queue subs
	for i := 0; i < 4; i++ {
		if _, err := sc.QueueSubscribe("foo", "bar", mcb, stan.DeliverAllAvailable()); err != nil {
			t.Fatalf("Unexpected error on subscribe: %v", err)
		}
	}
	// Wait for all messages to be received
	if err := Wait(doneCh); err != nil {
		t.Fatal("Did not get our messages")
	}
}
