package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"nft-forward/internal/daemonclient"
	"nft-forward/internal/nft"
)

// daemonClient is the subset of daemonclient.Client the TUI relies on.
// Declared locally so the TUI test suite can substitute a fake.
type daemonClient interface {
	Status() (daemonclient.StatusResp, error)
	ListRules() ([]daemonclient.Row, error)
	CreateRule(daemonclient.CreateRuleReq) (daemonclient.CreateRuleResp, error)
	UpdateRule(ref daemonclient.RuleRef, req daemonclient.UpdateRuleReq) error
	DeleteRule(ref daemonclient.RuleRef) error
}

type viewMode int

const (
	viewList viewMode = iota
	viewAdd
	viewEdit
	viewConfirmDelete
	viewConfirmClear
)

const (
	fProto    = 0
	fSrcPort  = 1
	fDestIP   = 2
	fDestPort = 3
	fComment  = 4
	fMode     = 5
)

// protoOptions is the ordered list of protocol choices for the selector.
var protoOptions = []string{"tcp", "udp", "tcp+udp"}

// modeOptions is the ordered list of data-plane mode choices for the selector.
// The zero index (kernel) is the default so that existing rules without an
// explicit Mode field behave identically to before.
var modeOptions = []string{nft.ModeKernel, nft.ModeUserspace}

type model struct {
	mode viewMode
	// daemon 与 rows 是最近一次从 daemon 读到的快照（loadSnapshot），只在启动和
	// refresh 时更新：daemon 是它的形态与会话状态，rows 是它在转发的全部规则。
	// 两者都可能在读完之后过时（会话断开、面板重新下发），所以它们只用于显示；
	// 能不能做某个操作由 daemon 在收到请求时裁决。
	daemon daemonclient.StatusResp
	rows   []daemonclient.Row
	cursor int

	inputs       []textinput.Model
	focusedInput int
	protoIdx     int // index into protoOptions; owned separately from inputs[fProto]
	modeIdx      int // index into modeOptions; owned separately from inputs[fMode]

	status string
	err    string

	width  int
	height int

	client daemonClient
}

// Run starts the TUI bound to the given daemon client. Caller (cmd) is
// responsible for verifying the daemon is reachable before invoking Run.
func Run(client daemonClient) error {
	st, rows, err := loadSnapshot(client)
	if err != nil {
		return err
	}
	p := tea.NewProgram(initialModel(client, st, rows), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func initialModel(client daemonClient, st daemonclient.StatusResp, rows []daemonclient.Row) model {
	return model{
		mode:   viewList,
		daemon: st,
		rows:   rows,
		inputs: buildInputs(),
		client: client,
	}
}

// loadSnapshot 从 daemon 读一次形态、会话状态与规则列表。
//
// 状态读不到按失败处理，不拿零值顶上：界面的标题要靠形态区分“本地模式”与“面板
// 托管”，状态缺失时显示哪一个都是在替 daemon 编一个它没说过的答案。
func loadSnapshot(client daemonClient) (daemonclient.StatusResp, []daemonclient.Row, error) {
	st, err := client.Status()
	if err != nil {
		return daemonclient.StatusResp{}, nil, fmt.Errorf("读取 daemon 状态失败: %w", err)
	}
	rows, err := client.ListRules()
	if err != nil {
		return daemonclient.StatusResp{}, nil, fmt.Errorf("加载规则失败: %w", err)
	}
	if rows == nil {
		rows = []daemonclient.Row{}
	}
	return st, rows, nil
}

func (m model) totalRows() int {
	return len(m.rows)
}

// rowAt 是第 i 行的规则内容，用于显示与预填表单。
func (m model) rowAt(i int) nft.Rule {
	return m.rows[i].Rule
}

// refAt 是第 i 行在 daemon 上的地址，改与删都把它原样交回。地址是 daemon 列规则时
// 给的（daemonclient.Row.Ref），这里不从规则的字段推断这一行是本地规则还是面板
// 规则。
func (m model) refAt(i int) daemonclient.RuleRef {
	return m.rows[i].Ref
}

// lockedFields returns the form field indices that stay read-only for the
// row being edited. Single-hop and local rows lock nothing — all fields are
// editable. Multi-hop chain rows lock proto+target (the relay skeleton owned
// by the server) but free listen_port/mode/comment.
func (m model) lockedFields() map[int]bool {
	if m.cursor >= len(m.rows) {
		return nil
	}
	r := m.rowAt(m.cursor)
	if r.HopCount > 1 {
		return map[int]bool{fProto: true, fDestIP: true, fDestPort: true}
	}
	return nil
}

func buildInputs() []textinput.Model {
	mk := func(placeholder string, width int) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.CharLimit = 64
		ti.Width = width
		return ti
	}
	// Slot 0 (fProto) and slot 5 (fMode) are placeholders so that the
	// focusedInput index constants (fProto=0 … fMode=5) remain valid and
	// cycleFocus arithmetic is unchanged. The slots are never rendered or
	// updated — their respective pill selectors are rendered from protoIdx /
	// modeIdx instead.
	return []textinput.Model{
		mk("", 0), // fProto placeholder
		mk("监听端口（空=自动）", 12),
		mk("目标 IP 或域名", 32),
		mk("目标端口", 12),
		mk("可选备注", 40),
		mk("", 0), // fMode placeholder
	}
}

func (m model) Init() tea.Cmd { return textinput.Blink }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		switch m.mode {
		case viewList:
			return m.updateList(msg)
		case viewAdd:
			return m.updateAdd(msg)
		case viewEdit:
			return m.updateEdit(msg)
		case viewConfirmDelete:
			return m.updateConfirmDelete(msg)
		case viewConfirmClear:
			return m.updateConfirmClear(msg)
		}
	}
	return m, nil
}

func (m *model) refresh() {
	st, rows, err := loadSnapshot(m.client)
	if err != nil {
		m.err = err.Error()
		return
	}
	m.daemon = st
	m.rows = rows
	if m.cursor >= m.totalRows() {
		m.cursor = m.totalRows() - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	m.status = "已从 daemon 重新加载"
}
