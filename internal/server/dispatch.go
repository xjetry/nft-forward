package server

import (
	"log"
	"sync"

	"nft-forward/internal/db"
)

// 下发：把一个节点此刻应有的规则集算出来、送过去、等它应答。
//
// 一条规则转不转发由 db.ActiveRuleHopsForPush 在下发那一刻按库里的当前值判定，节点
// 上的规则集只在下发时更新。所以面板里每个改了判定输入的入口，改完都要经过本文件
// 的某个 redispatch* 函数把受影响的节点重推一遍；本文件就是“改了什么 → 该重推谁”
// 的全部答案：
//
//	规则本身变了（建、改、删、重接）        dispatchNodes(这次改动涉及的节点)
//	账号级输入（停用、到期、全局配额与用量）  redispatchUser
//	一条授权的配额与用量                    redispatchUserGrant
//	一个落地出口的配额与用量                redispatchUserExit
//	节点启停                              redispatchThroughNode
//	时间流逝使账号到期                      Server.expiryEnforcer 定时重推

// nodeDispatchQueue 让同一个节点的下发一轮接一轮地进行，并把等待中的请求并进同一轮。
//
// 一轮（round）是“读库算出规则集 → 发给节点 → 等它应答”。两轮若交叠，先读库的那
// 一轮可能后发出去：节点最后应用的就是较旧的那份规则集，而两轮都以成功收场，面板
// 这边看不出异常，要等这个节点的下一次下发才纠正。队列保证两点：
//
//   - 同一节点的两轮不交叠，所以各轮的帧按读库的先后进入连接，节点最后应用的是最
//     晚一次读库的结果；
//   - 每个请求由一轮“在它提出之后才开始读库”的下发服务，所以调用方在调用前提交的
//     改动一定在这一轮读到的内容里。
//
// 一轮进行期间到达的请求不各跑一轮，而是共用紧接着的下一轮、拿同一个结果：它们要
// 的都是“库里此刻的内容到达节点”，一轮就够。不同节点之间互不等待。
type nodeDispatchQueue struct {
	round func(nodeID int64) error

	mu sync.Mutex
	// waiting 的键是正有 drain 协程在跑的节点；值是等下一轮的请求。节点的 drain
	// 协程发现没有等待者时把键删掉并退出，两步在同一次持锁里完成。
	waiting map[int64][]chan error
}

func newNodeDispatchQueue(round func(nodeID int64) error) *nodeDispatchQueue {
	return &nodeDispatchQueue{round: round, waiting: map[int64][]chan error{}}
}

// run 请求对 nodeID 下发一轮，返回服务这个请求的那一轮的结果。
func (q *nodeDispatchQueue) run(nodeID int64) error {
	done := make(chan error, 1)
	q.mu.Lock()
	_, draining := q.waiting[nodeID]
	q.waiting[nodeID] = append(q.waiting[nodeID], done)
	q.mu.Unlock()
	if !draining {
		go q.drain(nodeID)
	}
	return <-done
}

// drain 反复取走 nodeID 的全部等待者、跑一轮、把结果交给他们，直到没有等待者。
// 等待者是在这一轮开始读库之前取走的，这就是 run 承诺的“之后才开始读库”。
func (q *nodeDispatchQueue) drain(nodeID int64) {
	for {
		q.mu.Lock()
		waiters := q.waiting[nodeID]
		if len(waiters) == 0 {
			delete(q.waiting, nodeID)
			q.mu.Unlock()
			return
		}
		q.waiting[nodeID] = nil
		q.mu.Unlock()

		err := q.round(nodeID)
		for _, w := range waiters {
			w <- err
		}
	}
}

// dispatchToNode 把 nodeID 此刻应有的规则集下发给它，返回这次下发的结果。并发的
// 调用经 nodeDispatchQueue 串行、合并。
func (s *Server) dispatchToNode(nodeID int64) error {
	return s.dispatch.run(nodeID)
}

// dispatchRound 是对一个节点的一轮下发：按库里的当前值算出规则集，经节点的 agent
// 连接送过去并等它应答。所有节点（包括面板机上的本机节点）走同一条路；没有连接的
// 节点返回错误。只由 nodeDispatchQueue 调用，其余代码一律走 dispatchToNode。
//
// 结果记在 nodes 行上供页面显示同步状态：成功时记下应用时间并清掉上次的错误，
// 失败时记下错误、保留上次成功的应用时间。
func (s *Server) dispatchRound(nodeID int64) error {
	ruleHops, err := db.ActiveRuleHopsForPush(s.DB, nodeID)
	if err != nil {
		_ = db.MarkNodeDispatchError(s.DB, nodeID, err.Error())
		return err
	}
	rules := buildRules(s.DB, ruleHops)
	rev := computeRev(rules)
	warning, err := s.Hub.SendApplyRuleset(nodeID, rules, rev)
	if err != nil {
		_ = db.MarkNodeDispatchError(s.DB, nodeID, err.Error())
		return err
	}
	_ = db.MarkNodeApplied(s.DB, nodeID, warning)
	return nil
}

// dispatchNodes 依次下发 nodeIDs 并等每个节点应答。失败只记日志：调用方的改动已经
// 落库，节点的同步状态由 nodes 行上的 last_error 反映。why 说明是什么改动触发的。
func (s *Server) dispatchNodes(why string, nodeIDs []int64) {
	for _, n := range nodeIDs {
		if err := s.dispatchToNode(n); err != nil {
			log.Printf("dispatch node %d (%s): %v", n, why, err)
		}
	}
}

// redispatchNodes 在后台下发 nodeIDs，不等结果。给不能被下发阻塞的调用方用：Hub 的
// 读循环处理节点侧的规则命令与计数时，下发的应答要靠同一个读循环收。
func (s *Server) redispatchNodes(nodeIDs []int64) {
	for _, n := range nodeIDs {
		go s.dispatchNodes("规则变更", []int64{n})
	}
}

// redispatchUser 重新下发承载 userID 规则跳的每一个节点。改了这个用户的账号级判定
// 输入（停用标志、到期时间、全局配额、全局已用量）之后调用。
func (s *Server) redispatchUser(userID int64, why string) {
	nodes, err := db.DistinctUserNodes(s.DB, userID)
	if err != nil {
		log.Printf("redispatch user %d (%s): list nodes: %v", userID, why, err)
		return
	}
	s.dispatchNodes(why, nodes)
}

// redispatchUserGrant 重新下发 userID 经由授权节点 nodeID 的规则所在的每一个节点。
// 改了这条授权的配额或已用量之后调用。
func (s *Server) redispatchUserGrant(userID, nodeID int64, why string) {
	nodes, err := db.RulesAffectedByNode(s.DB, userID, nodeID)
	if err != nil {
		log.Printf("redispatch user %d grant %d (%s): list nodes: %v", userID, nodeID, why, err)
		return
	}
	s.dispatchNodes(why, nodes)
}

// redispatchUserExit 重新下发 userID 指向落地出口 host:port 的规则所在的每一个节点。
// 改了这个出口的配额、已用量或它是否仍在用户的落地集合里之后调用。
func (s *Server) redispatchUserExit(userID int64, host string, port int, why string) {
	nodes, err := db.NodesForUserExit(s.DB, userID, host, port)
	if err != nil {
		log.Printf("redispatch user %d exit %s:%d (%s): list nodes: %v", userID, host, port, why, err)
		return
	}
	s.dispatchNodes(why, nodes)
}

// redispatchThroughNode 重新下发“经过 nodeID 的规则”所在的每一个节点，nodeID 自己
// 也在其中。改了节点的启停之后调用：规则经过被停用的节点就整条不转发，所以链上的
// 其他节点也要跟着撤掉或恢复这条规则的跳。
func (s *Server) redispatchThroughNode(nodeID int64, why string) {
	nodes, err := db.NodesCarryingRulesThrough(s.DB, nodeID)
	if err != nil {
		log.Printf("redispatch through node %d (%s): list nodes: %v", nodeID, why, err)
		return
	}
	s.dispatchNodes(why, nodes)
}
