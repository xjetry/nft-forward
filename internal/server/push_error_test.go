package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"nft-forward/internal/db"
	"nft-forward/internal/wsproto"
)

// pushReply is what a fake node does with a frame the panel pushes it.
type pushReply int

const (
	replyAccept  pushReply = iota // acks it
	replyRefuse                   // acks it as failed
	replyGarbled                  // answers with something that is not an ack
	replySilent                   // does not answer
	replyHangUp                   // closes the connection
)

type pushEnv struct {
	d      *sql.DB
	s      *Server
	srv    *httptest.Server
	cookie *http.Cookie
	token  string
	nodes  int
}

func newPushEnv(t *testing.T) *pushEnv {
	t.Helper()
	d := openDB(t)
	s, err := newServer(d)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Router())
	t.Cleanup(srv.Close)
	hash, _ := HashPassword("pw")
	admin, err := db.CreateUser(d, "push-admin", hash, "admin")
	if err != nil {
		t.Fatal(err)
	}
	session, err := db.CreateSession(d, admin, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := db.CreateAPIToken(d, admin, "readwrite")
	if err != nil {
		t.Fatal(err)
	}
	return &pushEnv{d: d, s: s, srv: srv, cookie: &http.Cookie{Name: sessionCookie, Value: session}, token: token}
}

// post sends an empty POST as the admin: by session to an /api path, by
// token to an /api/v1 one.
func (e *pushEnv) post(t *testing.T, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(path, "/api/v1/") {
		req.Header.Set("Authorization", "Bearer "+e.token)
	} else {
		req.AddCookie(e.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

// offlineNode creates a node no agent is connected for.
func (e *pushEnv) offlineNode(t *testing.T) *db.Node {
	t.Helper()
	e.nodes++
	name := fmt.Sprintf("push-%d", e.nodes)
	n, err := db.CreateNode(e.d, name, "https://panel.example.com", "tok-"+name)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// connectedNode creates a node and connects a fake agent for it, which
// answers every apply_ruleset and upgrade frame as how says.
//
// Its hello reports the rev of an empty ruleset as applied. The node has no
// rules, so that is the rev the panel would push, and reconcileOnConnect
// pushes nothing: the first frame the agent gets is the one the test's own
// request caused.
func (e *pushEnv) connectedNode(t *testing.T, arch string, how pushReply) *db.Node {
	t.Helper()
	n := e.offlineNode(t)
	e.connectAgent(t, wsURL(e.srv), n, "tok-"+n.Name, arch, how)
	return n
}

// connectAgent 在入口 url 上为节点 n 接入一个假 agent，对之后收到的每一帧下发按 how
// 应答。token 是握手里带的凭据；本机入口不看凭据，传空串。握手上报的已应用 rev 的
// 作用见 connectedNode。
func (e *pushEnv) connectAgent(t *testing.T, url string, n *db.Node, token, arch string, how pushReply) {
	t.Helper()
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(websocket.StatusNormalClosure, "") })
	hello, _ := json.Marshal(wsproto.Hello{
		NodeToken: token, AgentVersion: "v0.0.1", AgentSHA: "stale", OS: "linux", Arch: arch,
		LastAppliedRev: computeRev(nil),
	})
	sendJSON(t, c, wsproto.Envelope{Type: wsproto.TypeHello, ID: "1", Payload: hello})
	var ack wsproto.HelloAck
	if env := recvEnvelope(t, c); json.Unmarshal(env.Payload, &ack) != nil || ack.Error != "" || ack.NodeID != n.ID {
		t.Fatalf("hello for %s rejected: %+v", n.Name, ack)
	}
	answer := func(typ, id string, payload any) {
		p, _ := json.Marshal(payload)
		b, _ := json.Marshal(wsproto.Envelope{Type: typ, ID: id, Payload: p})
		_ = c.Write(context.Background(), websocket.MessageText, b)
	}
	go func() {
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var env wsproto.Envelope
			if json.Unmarshal(b, &env) != nil {
				return
			}
			var ackType string
			var accepted, refused any
			switch env.Type {
			case wsproto.TypeApplyRuleset:
				ackType = wsproto.TypeApplyAck
				accepted, refused = wsproto.ApplyAck{OK: true}, wsproto.ApplyAck{Error: "nft: table busy"}
			case wsproto.TypeUpgrade:
				ackType = wsproto.TypeUpgradeAck
				accepted, refused = wsproto.UpgradeAck{OK: true}, wsproto.UpgradeAck{Error: "sha256 mismatch"}
			default:
				continue
			}
			switch how {
			case replyAccept:
				answer(ackType, env.ID, accepted)
			case replyRefuse:
				answer(ackType, env.ID, refused)
			case replyGarbled:
				answer(ackType, env.ID, "not an ack")
			case replySilent:
			case replyHangUp:
				c.Close(websocket.StatusGoingAway, "")
				return
			}
		}
	}()
}

// ackWithin shortens how long a push waits for its ack, for this test.
func ackWithin(t *testing.T, d time.Duration) {
	t.Helper()
	apply, upgrade := applyAckTimeout, upgradeAckTimeout
	applyAckTimeout, upgradeAckTimeout = d, d
	t.Cleanup(func() { applyAckTimeout, upgradeAckTimeout = apply, upgrade })
}

// In the test binary the release host is by default a local port that is not
// expected to be listening. A test that gets as far as fetching an agent
// binary without naming a host of its own (releaseHostAt) is refused there,
// and sends nothing to the real release host.
func init() { agentReleaseBase = "http://127.0.0.1:1" }

// releaseHostAt points the panel at a local release host for this test, so
// that fetching an agent binary goes nowhere else.
func releaseHostAt(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	prev := agentReleaseBase
	agentReleaseBase = srv.URL
	t.Cleanup(func() { agentReleaseBase = prev })
}

// v1Code is the error code of an /api/v1 reply, "" if it has none.
func v1Code(body string) string {
	var reply struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &reply)
	return reply.Error.Code
}

// TestPushFailureIsAnsweredByWhoseFailureItWas pins, for each way a push to
// a node fails, the answer of every route that reports a push: 409 where
// nothing could be sent, 502 where the node let the push down. The failures
// of the panel itself — a statement that fails on the way — are answered 500,
// which TestEveryStatementFailureIsReportedAsAFailure holds for these routes.
func TestPushFailureIsAnsweredByWhoseFailureItWas(t *testing.T) {
	e := newPushEnv(t)
	seedAgentArtifacts(t, "amd64")

	routes := []string{"/api/nodes/%d/resync", "/api/v1/nodes/%d/resync", "/api/nodes/%d/upgrade"}
	for _, c := range []struct {
		name   string
		node   func() *db.Node
		status int
		code   string
		says   map[string]string // per route: what the answer has to name
	}{
		{"a node that is not connected", func() *db.Node { return e.offlineNode(t) },
			http.StatusConflict, codeConflict, map[string]string{"": "节点未连接"}},
		{"a node that refuses the push", func() *db.Node { return e.connectedNode(t, "amd64", replyRefuse) },
			http.StatusBadGateway, codeUpstream, map[string]string{"resync": "nft: table busy", "upgrade": "sha256 mismatch"}},
		{"a node whose answer is not an ack", func() *db.Node { return e.connectedNode(t, "amd64", replyGarbled) },
			http.StatusBadGateway, codeUpstream, map[string]string{"": "malformed"}},
		{"a node that does not answer", func() *db.Node { return e.connectedNode(t, "amd64", replySilent) },
			http.StatusBadGateway, codeUpstream, map[string]string{"resync": "timeout", "upgrade": "超时"}},
		{"a node that hangs up", func() *db.Node { return e.connectedNode(t, "amd64", replyHangUp) },
			http.StatusBadGateway, codeUpstream, map[string]string{"resync": "closed", "upgrade": "断开"}},
	} {
		for _, route := range routes {
			t.Run(c.name+" "+route, func(t *testing.T) {
				if c.status == http.StatusBadGateway && strings.Contains(c.name, "does not answer") {
					ackWithin(t, 200*time.Millisecond)
				}
				// A node of its own: a push that was hung up on leaves the
				// node disconnected for whatever comes next.
				path := fmt.Sprintf(route, c.node().ID)
				status, body := e.post(t, path)
				if status != c.status {
					t.Fatalf("%s: %d %s, want %d", path, status, body, c.status)
				}
				if strings.HasPrefix(path, "/api/v1/") {
					if got := v1Code(body); got != c.code {
						t.Errorf("%s: code %q, want %q (%s)", path, got, c.code, body)
					}
				}
				says := c.says[""]
				for key, text := range c.says {
					if key != "" && strings.HasSuffix(route, key) {
						says = text
					}
				}
				if !strings.Contains(body, says) {
					t.Errorf("%s: the answer does not say %q: %s", path, says, body)
				}
			})
		}
	}

	// The same pushes, accepted: the fake node is one a push can succeed on,
	// so the failures above are the replies' doing.
	for _, route := range routes {
		path := fmt.Sprintf(route, e.connectedNode(t, "amd64", replyAccept).ID)
		if status, body := e.post(t, path); status != http.StatusOK {
			t.Errorf("%s to a node that accepts: %d %s, want 200", path, status, body)
		}
	}
}

// TestUpgradeWithoutABinaryToSendIsAnsweredByWhy: before anything is pushed,
// the panel needs the agent binary for the node's architecture. Having none
// to send — a dev build, which has no release — is how things stand (409);
// failing to get it from the release host is that host's failure (502).
func TestUpgradeWithoutABinaryToSendIsAnsweredByWhy(t *testing.T) {
	t.Run("a dev build has no release", func(t *testing.T) {
		e := newPushEnv(t)
		n := e.connectedNode(t, "amd64", replyAccept)
		status, body := e.post(t, fmt.Sprintf("/api/nodes/%d/upgrade", n.ID))
		if status != http.StatusConflict || !strings.Contains(body, "dev") {
			t.Errorf("%d %s, want 409 naming the dev build", status, body)
		}
	})
	t.Run("the release host does not have the binary", func(t *testing.T) {
		panelAt(t, testPanelVersion)
		asked := 0
		releaseHostAt(t, func(w http.ResponseWriter, r *http.Request) {
			asked++
			http.NotFound(w, r)
		})
		e := newPushEnv(t)
		n := e.connectedNode(t, "amd64", replyAccept)
		status, body := e.post(t, fmt.Sprintf("/api/nodes/%d/upgrade", n.ID))
		if status != http.StatusBadGateway || !strings.Contains(body, "status 404") {
			t.Errorf("%d %s, want 502 naming the host's 404", status, body)
		}
		if asked == 0 {
			t.Error("the local release host was not asked; the binary was looked for somewhere else")
		}
	})
	t.Run("the binary does not match its checksum", func(t *testing.T) {
		panelAt(t, testPanelVersion)
		releaseHostAt(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/SHA256SUMS") {
				fmt.Fprintf(w, "%064d  nft-agent\n", 0)
				return
			}
			_, _ = w.Write([]byte("not the released bytes"))
		})
		e := newPushEnv(t)
		n := e.connectedNode(t, "amd64", replyAccept)
		status, body := e.post(t, fmt.Sprintf("/api/nodes/%d/upgrade", n.ID))
		if status != http.StatusBadGateway || !strings.Contains(body, "校验失败") {
			t.Errorf("%d %s, want 502 naming the failed checksum", status, body)
		}
	})
}

// TestSelfNodePushFailureIsTheDaemonsFailure：self 节点的规则同样经 agent 通道送给面板机
// 上的 daemon，只是 daemon 从本机入口接入（Hub.ServeLocalWS）。所以它的下发失败与远程
// 节点是同一个回答：daemon 没连着，没有可下发的对象（409）；连着却拒绝应用，是 daemon
// 那一侧的失败（502）。
func TestSelfNodePushFailureIsTheDaemonsFailure(t *testing.T) {
	routes := []string{"/api/nodes/%d/resync", "/api/v1/nodes/%d/resync"}
	// daemon 接入一个带 self 节点的面板，对下发按 how 应答；how 为 nil 时不接入。
	panel := func(t *testing.T, how *pushReply) (*pushEnv, *db.Node) {
		t.Helper()
		e := newPushEnv(t)
		self, err := EnsureSelfNode(e.d)
		if err != nil {
			t.Fatal(err)
		}
		if how != nil {
			local := httptest.NewServer(e.s.LocalAgentHandler())
			t.Cleanup(local.Close)
			e.connectAgent(t, wsURL(local), self, "", "amd64", *how)
		}
		return e, self
	}
	refuse, accept := replyRefuse, replyAccept
	for _, c := range []struct {
		name   string
		how    *pushReply
		status int
		code   string
		says   string
	}{
		{"daemon 没有连着", nil, http.StatusConflict, codeConflict, "节点未连接"},
		{"daemon 拒绝应用", &refuse, http.StatusBadGateway, codeUpstream, "nft: table busy"},
		{"daemon 接受", &accept, http.StatusOK, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, self := panel(t, c.how)
			for _, route := range routes {
				path := fmt.Sprintf(route, self.ID)
				status, body := e.post(t, path)
				if status != c.status || !strings.Contains(body, c.says) {
					t.Errorf("%s: %d %s, want %d saying %q", path, status, body, c.status, c.says)
				}
				if strings.HasPrefix(path, "/api/v1/") {
					if got := v1Code(body); got != c.code {
						t.Errorf("%s: code %q, want %q (%s)", path, got, c.code, body)
					}
				}
			}
		})
	}
}

// TestPushErrAnswer: the fault travels with the error through wrapping, and
// an error nobody marked is the panel's own.
func TestPushErrAnswer(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{errNodeNotConnected, http.StatusConflict, codeConflict},
		{fmt.Errorf("node 7: %w", errNodeNotConnected), http.StatusConflict, codeConflict},
		{pushUpstream(errors.New("apply rejected")), http.StatusBadGateway, codeUpstream},
		{fmt.Errorf("fan-out: %w", pushUpstream(errors.New("timeout"))), http.StatusBadGateway, codeUpstream},
		{errors.New("database is locked"), http.StatusInternalServerError, codeInternal},
		{sql.ErrNoRows, http.StatusInternalServerError, codeInternal},
	} {
		if status, code := pushErrAnswer(c.err); status != c.status || code != c.code {
			t.Errorf("%v: %d %s, want %d %s", c.err, status, code, c.status, c.code)
		}
	}
	if got := pushUpstream(errors.New("apply rejected: boom")).Error(); got != "apply rejected: boom" {
		t.Errorf("a marked error reads %q, want the wrapped error's own text", got)
	}
}

// TestV1ErrorCodesAreTheOnesTheContractLists: the codes a reply can carry are
// the const block of apiv1.go; the codes a client is told to expect are the
// enum of Error.code in the OpenAPI document. Two lists in two files, and a
// code added to one alone is either undocumented or never sent.
func TestV1ErrorCodesAreTheOnesTheContractLists(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "apiv1.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sent := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "code") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal", name.Name)
				}
				code, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				sent[code] = true
			}
		}
	}
	if !sent[codeUpstream] || !sent[codeInternal] {
		t.Fatalf("the const block of apiv1.go was not read: %v", sent)
	}
	var doc struct {
		Components struct {
			Schemas struct {
				Error struct {
					Properties struct {
						Error struct {
							Properties struct {
								Code struct {
									Enum []string `json:"enum"`
								} `json:"code"`
							} `json:"properties"`
						} `json:"error"`
					} `json:"properties"`
				} `json:"Error"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openapiJSON, &doc); err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, code := range doc.Components.Schemas.Error.Properties.Error.Properties.Code.Enum {
		listed[code] = true
	}
	if len(listed) == 0 {
		t.Fatal("the OpenAPI document lists no error codes; the enum was not found")
	}
	for code := range sent {
		if !listed[code] {
			t.Errorf("code %q can be sent but is not in the OpenAPI enum", code)
		}
	}
	for code := range listed {
		if !sent[code] {
			t.Errorf("code %q is in the OpenAPI enum but apiv1.go declares no such code", code)
		}
	}
}
