package server

import (
	"net/http"

	"nft-forward/internal/db"
)

// validateCompositeMembers 检查要写给组合 rootID 的成员列表（新建组合时 rootID 为
// 0）。成员不存在或会成环返回 400 与原因，读拓扑失败返回 500；通过时 err 为 nil。
func (s *Server) validateCompositeMembers(rootID int64, members []int64) (status int, err error) {
	topo, err := db.LoadTopology(s.DB)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if err := topo.ValidateMembers(rootID, members); err != nil {
		return http.StatusBadRequest, err
	}
	return 0, nil
}
