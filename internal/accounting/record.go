package accounting

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"nft-forward/internal/db"
)

// Result 是一批计数入账之后，调用方（Hub）还要据此行动的事。
type Result struct {
	// RolledOver 是这一批入账之前周期翻转、账本被清零的属主。配额条件只在下发时
	// 求值，调用方要重推他们的节点，因配额用尽被排除的规则才会在新周期恢复。
	RolledOver []int64
	// QuotaOwners 是这一批向其授权账或落地出口账记过账的属主（见
	// Postings.QuotaOwners）。只在这一批成功落库时非空。
	QuotaOwners []int64
	// Unmatched 是匹配不到任何规则跳的样本。
	Unmatched []Sample

	book *Book
}

// OwnerOf 返回样本所在规则的属主；匹配不到规则跳、无属主，或这一批没能读出计价
// 信息时为 0。
func (r *Result) OwnerOf(proto string, port int) int64 {
	return r.book.OwnerOf(CounterKey{Proto: proto, Port: port})
}

// Record 把节点 nodeID 上报的一批样本记进各本账：读出计价信息（LoadBook）、算出
// 增量（Meter）、为这一批要记账的属主做周期翻转、在一个事务里落库。
//
// 计价信息读不出来时整批不记，返回的 Result 是空的：一部分来源缺失时照记，会按
// 错误的倍率、方向或位置记账。周期翻转出错的属主跳过翻转、照常落库，错误随返回值
// 一并报告。
func Record(d *sql.DB, nodeID int64, samples []Sample) (*Result, error) {
	book, err := LoadBook(d, nodeID)
	if err != nil {
		return &Result{}, fmt.Errorf("load book: %w", err)
	}
	p := Meter(book, samples)
	res := &Result{Unmatched: p.Unmatched, book: book}

	// 翻转在落库之前、在事务之外做：db.Open 把连接池限成一条连接，在 post 的事务里
	// 再走连接池读写，就要等事务自己占着的那条连接，永远等不到。
	var errs []error
	for _, uid := range sortedIDs(p.Charged) {
		u, err := db.GetUserByID(d, uid)
		if err != nil {
			errs = append(errs, fmt.Errorf("user %d: %w", uid, err))
			continue
		}
		rolled, err := ResetCycleIfDue(d, u)
		if err != nil {
			errs = append(errs, fmt.Errorf("user %d cycle reset: %w", uid, err))
			continue
		}
		if rolled {
			res.RolledOver = append(res.RolledOver, uid)
		}
	}

	if err := post(d, nodeID, p); err != nil {
		errs = append(errs, err)
		return res, errors.Join(errs...)
	}
	res.QuotaOwners = p.QuotaOwners()
	return res, errors.Join(errs...)
}

// post 把一批增量在一个事务里写进各本账：要么全部记上，要么全部不记，各本账之间
// 不会出现一本记了、另一本没记的批次。规则、授权、落地出口在读取与写入之间被删的，
// 对应的 UPDATE 影响 0 行，这一批在它们身上的增量随之丢弃。上报节点自己被删时，
// 插入 node_raw_traffic 撞上外键，整批不记——这台节点上的跳行也已随它级联删除。
func post(d *sql.DB, nodeID int64, p *Postings) error {
	if p.empty() {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if p.NodeRaw > 0 {
		if _, err := tx.Exec(`INSERT INTO node_raw_traffic(node_id, raw_bytes) VALUES(?,?)
			ON CONFLICT(node_id) DO UPDATE SET raw_bytes = raw_bytes + excluded.raw_bytes`,
			nodeID, p.NodeRaw); err != nil {
			return fmt.Errorf("node %d raw: %w", nodeID, err)
		}
	}
	for id, f := range p.Hops {
		if _, err := tx.Exec(`UPDATE rule_hops SET total_bytes = total_bytes + ? WHERE id=?`, f, id); err != nil {
			return fmt.Errorf("rule hop %d: %w", id, err)
		}
	}
	for id, w := range p.Rules {
		if _, err := tx.Exec(`UPDATE rules SET weighted_bytes = weighted_bytes + ? WHERE id=?`, w, id); err != nil {
			return fmt.Errorf("rule %d: %w", id, err)
		}
	}
	for k, f := range p.Grants {
		if _, err := tx.Exec(`UPDATE user_nodes SET traffic_used_bytes = traffic_used_bytes + ? WHERE user_id=? AND node_id=?`,
			f, k.User, k.Node); err != nil {
			return fmt.Errorf("user %d grant %d: %w", k.User, k.Node, err)
		}
	}
	for uid, w := range p.Users {
		if _, err := tx.Exec(`UPDATE users SET traffic_used_bytes = traffic_used_bytes + ? WHERE id=?`, w, uid); err != nil {
			return fmt.Errorf("user %d: %w", uid, err)
		}
	}
	now := time.Now().Unix()
	for k, e := range p.Exits {
		if _, err := tx.Exec(`UPDATE user_landing_exits SET used_bytes = used_bytes + ?, used_bytes_up = used_bytes_up + ?, used_bytes_down = used_bytes_down + ?, updated_at = ?
			WHERE user_id=? AND host=? AND port=?`,
			e.Raw(), e.Up, e.Down, now, k.User, k.Host, k.Port); err != nil {
			return fmt.Errorf("user %d exit %s:%d: %w", k.User, k.Host, k.Port, err)
		}
	}
	return tx.Commit()
}
