package accounting

import (
	"database/sql"

	"nft-forward/internal/db"
)

// bookSQL 用一条语句从上报节点的每一行 rule_hops 解析出它的全部计价来源与位置，
// 单条 SELECT 在 SQLite 里读的是同一个快照，这些值彼此一致。
//
//   - rn：上报节点自己——是否单向计费。
//   - en：规则的逻辑入口节点（rules.node_id）——倍率。规则引用的是组合时就是组合本身。
//   - u：规则属主——费率。无属主时为 NULL。
//   - 末跳、段首由同一规则的其它跳行算出；落地出口看属主名下有没有在册的同一出口。
const bookSQL = `
SELECT rh.id, rh.rule_id, rh.proto, rh.listen_port, rh.logical_node_id,
       rh.position = 0,
       rh.position = (SELECT MAX(f.position) FROM rule_hops f WHERE f.rule_id = rh.rule_id),
       rh.position = (SELECT MIN(s.position) FROM rule_hops s
                      WHERE s.rule_id = rh.rule_id AND s.logical_node_id = rh.logical_node_id),
       rn.unidirectional,
       r.owner_id, en.rate_multiplier, u.billing_rate,
       r.exit_host, r.exit_port,
       EXISTS (SELECT 1 FROM user_landing_exits e
               WHERE e.user_id = r.owner_id AND e.host = r.exit_host AND e.port = r.exit_port AND e.present = 1)
FROM rule_hops rh
JOIN nodes rn ON rn.id = rh.node_id
JOIN rules r ON r.id = rh.rule_id
LEFT JOIN nodes en ON en.id = r.node_id
LEFT JOIN users u ON u.id = r.owner_id
WHERE rh.node_id = ?`

// LoadBook 读出节点 nodeID 上每一跳的计价条件与位置。
func LoadBook(d db.DBTX, nodeID int64) (*Book, error) {
	rows, err := d.Query(bookSQL, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b := &Book{Hops: map[CounterKey]*Hop{}}
	for rows.Next() {
		var (
			h                     Hop
			proto, exitHost       string
			port, exitPort        int
			uni, exitPresent      bool
			owner                 sql.NullInt64
			multiplier, rate      sql.NullFloat64
			entry, final, segHead bool
		)
		if err := rows.Scan(&h.ID, &h.RuleID, &proto, &port, &h.Place.Segment,
			&entry, &final, &segHead, &uni,
			&owner, &multiplier, &rate, &exitHost, &exitPort, &exitPresent); err != nil {
			return nil, err
		}
		h.Place.Entry, h.Place.Final, h.Place.SegmentStart = entry, final, segHead
		h.Owner = owner.Int64
		h.Pricing = pricing(uni, owner.Valid, multiplier, rate)
		if owner.Valid && exitPresent {
			h.Exit = &ExitKey{User: owner.Int64, Host: exitHost, Port: exitPort}
		}
		// RegenerateRule 分配端口时按跨协议占用（db.OccupiedPortsOnNode）避开同一台
		// 节点上已有的跳，而 CounterProtos 给出的协议都在占用范围之内，所以两跳不会
		// 登记到同一个计数键上。
		for _, p := range db.CounterProtos(proto) {
			b.Hops[CounterKey{Proto: p, Port: port}] = &h
		}
	}
	return b, rows.Err()
}

// pricing 给一跳的计价条件落缺省值：
//
//   - 无属主的规则不向任何人计价，倍率与费率都取 1：规则账上记的就是折算字节。
//   - 倍率 0 是有意设置的免费节点，原样保留；负数是非法值、读不到（入口节点的行
//     不存在）也按 1——读不到倍率不能让一条规则悄悄变成免费。
//   - 费率 ≤ 0 按 1。
func pricing(unidirectional, owned bool, multiplier, rate sql.NullFloat64) Pricing {
	p := Pricing{Unidirectional: unidirectional, Multiplier: 1, Rate: 1}
	if !owned {
		return p
	}
	if multiplier.Valid && multiplier.Float64 >= 0 {
		p.Multiplier = multiplier.Float64
	}
	if rate.Valid && rate.Float64 > 0 {
		p.Rate = rate.Float64
	}
	return p
}
