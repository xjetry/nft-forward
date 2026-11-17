package server

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// queuedWaiters 报告 nodeID 上此刻有几个请求在等下一轮。
func queuedWaiters(q *nodeDispatchQueue, nodeID int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting[nodeID])
}

func waitQueued(t *testing.T, q *nodeDispatchQueue, nodeID int64, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for queuedWaiters(q, nodeID) != want {
		if time.Now().After(deadline) {
			t.Fatalf("节点 %d 上等待下一轮的请求数应为 %d，实际 %d", nodeID, want, queuedWaiters(q, nodeID))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// 一轮下发是“读库 → 发给节点”。两次下发交叠时，先读库的那一轮不能后发出去，否则
// 节点最后拿到的是旧内容。这里让第一轮读完库之后卡住，其间库变了并来了第二个
// 请求：节点收到的最后一份必须是新内容，而且第二个请求返回时新内容已经送达。
func TestNodeDispatchLaterRequestDeliversLaterState(t *testing.T) {
	var (
		mu       sync.Mutex
		state    = "旧"
		received []string
	)
	firstRead := make(chan struct{})
	release := make(chan struct{})
	var rounds atomic.Int32
	q := newNodeDispatchQueue(func(int64) error {
		mu.Lock()
		snapshot := state
		mu.Unlock()
		if rounds.Add(1) == 1 {
			close(firstRead)
			<-release
		}
		mu.Lock()
		received = append(received, snapshot)
		mu.Unlock()
		return nil
	})

	firstDone := make(chan error, 1)
	go func() { firstDone <- q.run(7) }()
	<-firstRead // 第一轮已读到“旧”，还没发出去

	mu.Lock()
	state = "新"
	mu.Unlock()
	secondDone := make(chan error, 1)
	go func() { secondDone <- q.run(7) }()

	// 第二个请求若在第一轮结束前就返回，说明它另起了一轮、抢在旧内容之前把新内容
	// 发了出去——随后第一轮发出的旧内容会盖掉它。
	select {
	case <-secondDone:
		t.Fatal("第一轮还没把旧内容发出去，第二个请求就返回了：两轮交叠，旧内容将后到并盖掉新内容")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 || received[0] != "旧" || received[1] != "新" {
		t.Fatalf("节点应先收到旧内容、最后收到新内容，实际 %v", received)
	}
}

// 一轮进行期间到达的请求共用紧接着的下一轮：不管来了几个，只多跑一轮，而且每个
// 请求拿到的都是那一轮的结果。
func TestNodeDispatchCoalescesRequestsThatArriveDuringARound(t *testing.T) {
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	var rounds atomic.Int32
	secondRoundErr := errors.New("第二轮的结果")
	q := newNodeDispatchQueue(func(int64) error {
		switch rounds.Add(1) {
		case 1:
			close(firstStarted)
			<-release
			return nil
		default:
			return secondRoundErr
		}
	})

	firstDone := make(chan error, 1)
	go func() { firstDone <- q.run(7) }()
	<-firstStarted

	const late = 5
	lateDone := make(chan error, late)
	for i := 0; i < late; i++ {
		go func() { lateDone <- q.run(7) }()
	}
	waitQueued(t, q, 7, late)
	close(release)

	if err := <-firstDone; err != nil {
		t.Fatalf("第一个请求应拿到第一轮的结果（成功），实际 %v", err)
	}
	for i := 0; i < late; i++ {
		if err := <-lateDone; !errors.Is(err, secondRoundErr) {
			t.Fatalf("第一轮期间到达的请求应拿到第二轮的结果，实际 %v", err)
		}
	}
	if n := rounds.Load(); n != 2 {
		t.Fatalf("第一轮期间到达的 %d 个请求应并成一轮，共 2 轮，实际 %d 轮", late, n)
	}
	// 没有等待者之后队列不留下这个节点的状态，下一个请求照常起新的一轮。
	if err := q.run(7); !errors.Is(err, secondRoundErr) {
		t.Fatalf("队列排空后的新请求应起新的一轮，实际 %v", err)
	}
	if n := rounds.Load(); n != 3 {
		t.Fatalf("排空后的新请求应是第 3 轮，实际 %d 轮", n)
	}
}

// 不同节点的下发互不等待：一个节点的一轮卡住时，另一个节点照常下发。
func TestNodeDispatchDoesNotMakeNodesWaitForEachOther(t *testing.T) {
	slowStarted := make(chan struct{})
	release := make(chan struct{})
	q := newNodeDispatchQueue(func(nodeID int64) error {
		if nodeID == 1 {
			close(slowStarted)
			<-release
		}
		return nil
	})
	slowDone := make(chan error, 1)
	go func() { slowDone <- q.run(1) }()
	<-slowStarted

	otherDone := make(chan error, 1)
	go func() { otherDone <- q.run(2) }()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("节点 1 的一轮卡住时，节点 2 的下发不应被挡住")
	}
	close(release)
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
}
