// Package landing parses proxy-subscription URIs into landing nodes and
// rewrites their connection endpoint. It is panel-agnostic: any subscription
// that returns a base64 (or plain) list of proxy URIs works, and manually
// pasted URIs are parsed the same way. RewriteEndpoint swaps only the host:port
// a client dials — keeping every other field (TLS params, name, credentials)
// intact — so a relay's entry endpoint can stand in for the real landing one.
package landing

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Node is a single parsed landing node.
type Node struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	URI      string `json:"uri"`
}

// ParseURIs parses each proxy URI, skipping blank lines, comments (lines
// starting with '#') and anything that doesn't parse into host:port.
func ParseURIs(uris []string) []Node {
	out := make([]Node, 0, len(uris))
	for _, raw := range uris {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		if n, ok := parseOne(raw); ok {
			n.Name = StripDedupSuffix(n.Name)
			out = append(out, n)
		}
	}
	return out
}

// dedupSuffixRe matches the trailing "^~2~^"-style counters some panels (e.g.
// Remnawave) append when a subscription carries same-named nodes — typically
// the same node exported once per protocol. Nodes are identified by host:port
// everywhere downstream, so the counter is display noise.
var dedupSuffixRe = regexp.MustCompile(`(\s*\^~\d+~\^)+$`)

// StripDedupSuffix drops the trailing dedup counter, keeping the name as-is
// when the counter is all there is (an empty name would render worse).
func StripDedupSuffix(name string) string {
	out := strings.TrimSpace(dedupSuffixRe.ReplaceAllString(name, ""))
	if out == "" {
		return name
	}
	return out
}

func parseOne(uri string) (Node, bool) {
	idx := strings.Index(uri, "://")
	if idx <= 0 {
		return parseSnell(uri)
	}
	scheme := strings.ToLower(uri[:idx])
	switch scheme {
	case "vmess":
		return parseVMess(uri)
	case "ss":
		return parseSS(uri)
	case "http", "https":
		return Node{}, false
	default:
		return parseAuthority(uri, normProto(scheme))
	}
}

// normProto folds scheme aliases onto a canonical protocol label.
func normProto(scheme string) string {
	switch scheme {
	case "hy2":
		return "hysteria2"
	default:
		return scheme
	}
}

// parseAuthority handles the common scheme://[userinfo@]host:port?query#name
// shape (vless, trojan, tuic, hysteria2, ...). A numeric port is required, so
// portless URLs (e.g. http://host) are rejected by the caller's port check.
func parseAuthority(uri, proto string) (Node, bool) {
	u, err := url.Parse(uri)
	if err != nil {
		return Node{}, false
	}
	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil || host == "" || port < 1 || port > 65535 {
		return Node{}, false
	}
	return Node{Name: u.Fragment, Protocol: proto, Host: host, Port: port, URI: uri}, true
}

// parseSnell handles Surge-style snell config lines:
//   Name = snell, host, port, psk = xxx, version = 5, ...
func parseSnell(line string) (Node, bool) {
	eqIdx := strings.Index(line, "=")
	if eqIdx < 0 {
		return Node{}, false
	}
	name := strings.TrimSpace(line[:eqIdx])
	rest := strings.TrimSpace(line[eqIdx+1:])
	parts := strings.SplitN(rest, ",", -1)
	if len(parts) < 3 || strings.TrimSpace(strings.ToLower(parts[0])) != "snell" {
		return Node{}, false
	}
	host := strings.TrimSpace(parts[1])
	port, err := strconv.Atoi(strings.TrimSpace(parts[2]))
	if err != nil || host == "" || port < 1 || port > 65535 {
		return Node{}, false
	}
	return Node{Name: name, Protocol: "snell", Host: host, Port: port, URI: line}, true
}

func parseVMess(uri string) (Node, bool) {
	dec, ok := b64Decode(uri[len("vmess://"):])
	if !ok {
		return Node{}, false
	}
	var m map[string]any
	if err := json.Unmarshal(dec, &m); err != nil {
		return Node{}, false
	}
	host, _ := m["add"].(string)
	port := jsonPort(m["port"])
	if host == "" || port < 1 || port > 65535 {
		return Node{}, false
	}
	name, _ := m["ps"].(string)
	return Node{Name: name, Protocol: "vmess", Host: host, Port: port, URI: uri}, true
}

// parseSS handles both SIP002 (ss://base64(method:pass)@host:port) and the
// legacy whole-base64 form (ss://base64(method:pass@host:port)).
func parseSS(uri string) (Node, bool) {
	rest := uri[len("ss://"):]
	name := ""
	if h := strings.Index(rest, "#"); h >= 0 {
		name, _ = url.PathUnescape(rest[h+1:])
		rest = rest[:h]
	}
	if q := strings.Index(rest, "?"); q >= 0 {
		rest = rest[:q] // drop plugin query; it has no bearing on host:port
	}
	var hostport string
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		hostport = rest[at+1:]
	} else {
		dec, ok := b64Decode(rest)
		if !ok {
			return Node{}, false
		}
		at2 := strings.LastIndex(string(dec), "@")
		if at2 < 0 {
			return Node{}, false
		}
		hostport = string(dec)[at2+1:]
	}
	host, portStr, err := net.SplitHostPort(hostport)
	if err != nil {
		return Node{}, false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || host == "" || port < 1 || port > 65535 {
		return Node{}, false
	}
	return Node{Name: name, Protocol: "ss", Host: host, Port: port, URI: uri}, true
}

// RewriteEndpoint returns uri with its connection host:port replaced by
// newHost:newPort, preserving every other component byte-for-byte where
// possible (vmess/ss-legacy must re-encode their payload).
func RewriteEndpoint(uri, newHost string, newPort int) (string, error) {
	idx := strings.Index(uri, "://")
	if idx <= 0 {
		return rewriteSnell(uri, newHost, newPort)
	}
	switch strings.ToLower(uri[:idx]) {
	case "vmess":
		return rewriteVMess(uri, newHost, newPort)
	case "ss":
		return rewriteSS(uri, newHost, newPort)
	default:
		return rewriteAuthority(uri, newHost, newPort)
	}
}

// rewriteAuthority replaces the host:port of an authority-style URI without
// touching the userinfo, path, query or fragment — those are copied verbatim so
// no re-encoding can alter them.
func rewriteAuthority(uri, newHost string, newPort int) (string, error) {
	idx := strings.Index(uri, "://")
	prefix := uri[:idx+3]
	rest := uri[idx+3:]
	end := len(rest)
	for i := 0; i < len(rest); i++ {
		if c := rest[i]; c == '/' || c == '?' || c == '#' {
			end = i
			break
		}
	}
	authority, tail := rest[:end], rest[end:]
	userinfo := ""
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		userinfo = authority[:at+1]
	}
	return prefix + userinfo + net.JoinHostPort(newHost, strconv.Itoa(newPort)) + tail, nil
}

func rewriteSnell(line, newHost string, newPort int) (string, error) {
	eqIdx := strings.Index(line, "=")
	if eqIdx < 0 {
		return "", errInvalid
	}
	rest := strings.TrimSpace(line[eqIdx+1:])
	parts := strings.SplitN(rest, ",", -1)
	if len(parts) < 3 || strings.TrimSpace(strings.ToLower(parts[0])) != "snell" {
		return "", errInvalid
	}
	parts[1] = " " + newHost
	parts[2] = " " + strconv.Itoa(newPort)
	return line[:eqIdx+1] + " " + strings.Join(parts, ","), nil
}

func rewriteVMess(uri string, newHost string, newPort int) (string, error) {
	dec, ok := b64Decode(uri[len("vmess://"):])
	if !ok {
		return "", errInvalid
	}
	var m map[string]any
	if err := json.Unmarshal(dec, &m); err != nil {
		return "", errInvalid
	}
	m["add"] = newHost
	m["port"] = strconv.Itoa(newPort)
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(b), nil
}

func rewriteSS(uri string, newHost string, newPort int) (string, error) {
	rest := uri[len("ss://"):]
	frag := ""
	if h := strings.Index(rest, "#"); h >= 0 {
		frag = rest[h:]
		rest = rest[:h]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		// SIP002: authority style, host:port is in the clear.
		return rewriteAuthority(uri, newHost, newPort)
	}
	// Legacy: the whole payload (method:pass@host:port) is base64-encoded.
	query := ""
	if q := strings.Index(rest, "?"); q >= 0 {
		query = rest[q:]
		rest = rest[:q]
	}
	dec, ok := b64Decode(rest)
	if !ok {
		return "", errInvalid
	}
	s := string(dec)
	at := strings.LastIndex(s, "@")
	if at < 0 {
		return "", errInvalid
	}
	cred := s[:at+1]
	payload := cred + net.JoinHostPort(newHost, strconv.Itoa(newPort))
	return "ss://" + base64.StdEncoding.EncodeToString([]byte(payload)) + query + frag, nil
}

// DecodeSubscription turns a subscription response body into the list of proxy
// URI lines it carries, accepting either a base64-encoded blob or plain text.
func DecodeSubscription(body []byte) []string {
	text := strings.TrimSpace(string(body))
	if dec, ok := b64Decode(stripWhitespace(text)); ok && strings.Contains(string(dec), "://") {
		text = string(dec)
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func stripWhitespace(s string) string {
	return strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(s)
}

// b64Decode tries the standard and URL alphabets, with and without padding.
func b64Decode(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

func jsonPort(v any) int {
	switch p := v.(type) {
	case string:
		n, _ := strconv.Atoi(p)
		return n
	case float64:
		return int(p)
	}
	return 0
}

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

const errInvalid = sentinelErr("invalid proxy uri")
