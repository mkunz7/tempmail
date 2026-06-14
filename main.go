package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

const (
	maxMessageBytes    = 35 << 20
	maxAttachmentBytes = 24 << 20
)

//go:embed web/*
var webFS embed.FS

type config struct {
	Domain     string
	Domains    map[string]bool
	HTTPAddr   string
	BasePath   string
	SubmitAddr string
	SubmitName string
	MaddyBin   string
	AdminUser  string
	AdminPass  string
	AdminHash  string
	Protected  map[string]bool
	InboxTTL   time.Duration
	GenHash    bool
}

type app struct {
	cfg     config
	mu      sync.Mutex
	inboxes map[string]*session
}

type session struct {
	Address string
	Pass    string
	Created time.Time
	Seen    time.Time
}

type inboxResponse struct {
	Address  string    `json:"address"`
	Created  time.Time `json:"created"`
	Seen     time.Time `json:"seen"`
	Messages []message `json:"messages"`
}

type statsResponse struct {
	Generated time.Time   `json:"generated"`
	Inboxes   []inboxStat `json:"inboxes"`
}

type inboxStat struct {
	Address      string    `json:"address"`
	Created      time.Time `json:"created"`
	Seen         time.Time `json:"seen"`
	MessageCount int       `json:"messageCount"`
	Error        string    `json:"error,omitempty"`
}

type message struct {
	ID          string       `json:"id"`
	UID         uint32       `json:"uid"`
	From        string       `json:"from"`
	To          []string     `json:"to"`
	DeliveredTo string       `json:"deliveredTo"`
	Subject     string       `json:"subject"`
	Date        time.Time    `json:"date"`
	Text        string       `json:"text"`
	HTML        string       `json:"html"`
	Attachments []attachment `json:"attachments"`
}

type attachment struct {
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	Index       int    `json:"index"`
}

var localPartRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._+-]{0,62}$`)

func normalizeLocalPart(local string) string {
	local = strings.ToLower(strings.TrimSpace(local))
	if i := strings.Index(local, "+"); i >= 0 {
		local = local[:i]
	}
	return strings.ReplaceAll(local, ".", "")
}

func normalizeVariantAddress(address string) (string, bool) {
	local, domain, ok := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	if !ok {
		return "", false
	}
	local = normalizeLocalPart(local)
	if local == "" || domain == "" {
		return "", false
	}
	return local + "@" + domain, true
}

func main() {
	cfg := loadConfig()
	if cfg.GenHash {
		if err := generatePasswordHash(); err != nil {
			log.Fatal(err)
		}
		return
	}
	a := &app{cfg: cfg, inboxes: map[string]*session{}}
	if cfg.Domain == "" || len(cfg.Domains) == 0 {
		log.Fatal("configure at least one mail domain with TEMPMAIL_DOMAIN, TEMPMAIL_DOMAINS, -domain, or -domains")
	}
	a.removeStaleCatchAlls()
	go a.reaper()

	log.Printf("tempmail listening on %s for domains %s under %s", cfg.HTTPAddr, strings.Join(sortedDomains(cfg.Domains), ","), cfg.BasePath)
	if err := http.ListenAndServe(cfg.HTTPAddr, a.routes()); err != nil {
		log.Fatal(err)
	}
}

func loadConfig() config {
	var cfg config
	flag.StringVar(&cfg.Domain, "domain", getenv("TEMPMAIL_DOMAIN", ""), "primary mail domain")
	domains := flag.String("domains", getenv("TEMPMAIL_DOMAINS", ""), "comma-separated allowed mail domains")
	flag.StringVar(&cfg.HTTPAddr, "http", getenv("TEMPMAIL_HTTP_ADDR", "127.0.0.1:3005"), "HTTP listen address")
	flag.StringVar(&cfg.BasePath, "base-path", getenv("TEMPMAIL_BASE_PATH", "/mail"), "public base path")
	flag.StringVar(&cfg.SubmitAddr, "submit", getenv("TEMPMAIL_SUBMIT_ADDR", "127.0.0.1:587"), "local Maddy submission address")
	flag.StringVar(&cfg.SubmitName, "submit-name", getenv("TEMPMAIL_SUBMIT_SERVER_NAME", ""), "optional TLS server name for Maddy submission")
	flag.StringVar(&cfg.MaddyBin, "maddy", getenv("TEMPMAIL_MADDY_BIN", "/usr/local/bin/maddy"), "maddy binary path")
	flag.StringVar(&cfg.AdminUser, "user", getenv("TEMPMAIL_ADMIN_USER", ""), "optional HTTP basic auth user")
	flag.StringVar(&cfg.AdminPass, "pass", getenv("TEMPMAIL_ADMIN_PASS", ""), "optional HTTP basic auth password")
	flag.StringVar(&cfg.AdminHash, "pass-hash", getenv("TEMPMAIL_ADMIN_PASS_HASH", ""), "optional bcrypt hash for HTTP basic auth password")
	protected := flag.String("protected", getenv("TEMPMAIL_PROTECTED_LOCALPARTS", "postmaster,abuse,hostmaster,webmaster"), "comma-separated local-parts whose mailbox contents are preserved")
	flag.BoolVar(&cfg.GenHash, "genhash", false, "prompt for a password and print a bcrypt hash")
	ttl := flag.Duration("ttl", envDuration("TEMPMAIL_INBOX_TTL", 20*time.Minute), "inactive inbox TTL")
	flag.Parse()

	cfg.Domain = strings.ToLower(strings.TrimSpace(cfg.Domain))
	cfg.Domains = parseDomains(*domains, cfg.Domain)
	if cfg.Domain == "" {
		cfg.Domain = firstDomain(*domains)
	}
	cfg.Protected = parseLocalParts(*protected)
	cfg.BasePath = "/" + strings.Trim(strings.TrimSpace(cfg.BasePath), "/")
	if cfg.BasePath == "/" {
		cfg.BasePath = ""
	}
	cfg.InboxTTL = *ttl
	return cfg
}

func generatePasswordHash() error {
	var password []byte
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Password: ")
		first, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "Confirm password: ")
		second, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(first, second) != 1 {
			return errors.New("passwords do not match")
		}
		password = first
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		password = []byte(strings.TrimRight(line, "\r\n"))
	}
	if len(password) == 0 {
		return errors.New("password cannot be empty")
	}
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	fmt.Printf("TEMPMAIL_ADMIN_PASS_HASH=%s\n", hash)
	return nil
}

func parseDomains(value, fallback string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		domain := strings.ToLower(strings.TrimSpace(part))
		if domain != "" {
			out[domain] = true
		}
	}
	if fallback != "" {
		out[fallback] = true
	}
	return out
}

func firstDomain(value string) string {
	for _, part := range strings.Split(value, ",") {
		domain := strings.ToLower(strings.TrimSpace(part))
		if domain != "" {
			return domain
		}
	}
	return ""
}

func sortedDomains(domains map[string]bool) []string {
	out := make([]string, 0, len(domains))
	for domain := range domains {
		out = append(out, domain)
	}
	sort.Strings(out)
	return out
}

func parseLocalParts(value string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			out[part] = true
		}
	}
	return out
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/stats", a.handleStats)
	mux.HandleFunc("/api/inbox", a.handleInbox)
	mux.HandleFunc("/api/inbox/", a.handleInboxItem)
	mux.HandleFunc("/api/send", a.handleSend)
	mux.HandleFunc("/api/stats", a.handleStatsAPI)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })

	var h http.Handler = mux
	if a.cfg.BasePath != "" {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == a.cfg.BasePath {
				http.Redirect(w, r, a.cfg.BasePath+"/", http.StatusMovedPermanently)
				return
			}
			if !strings.HasPrefix(r.URL.Path, a.cfg.BasePath+"/") {
				http.NotFound(w, r)
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = strings.TrimPrefix(r.URL.Path, a.cfg.BasePath)
			h := http.StripPrefix("", mux)
			h.ServeHTTP(w, r2)
		})
	}
	return a.auth(h)
}

func (a *app) auth(next http.Handler) http.Handler {
	if a.cfg.AdminUser == "" && a.cfg.AdminPass == "" && a.cfg.AdminHash == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(a.cfg.AdminUser)) != 1 || !a.validAdminPassword(p) {
			w.Header().Set("WWW-Authenticate", `Basic realm="tempmail"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) validAdminPassword(password string) bool {
	if a.cfg.AdminHash != "" {
		return bcrypt.CompareHashAndPassword([]byte(a.cfg.AdminHash), []byte(password)) == nil
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(a.cfg.AdminPass)) == 1
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	t, err := template.ParseFS(webFS, "web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = t.Execute(w, map[string]string{
		"Domain":   a.domainForRequest(r),
		"BasePath": a.cfg.BasePath,
		"InboxTTL": humanDuration(a.cfg.InboxTTL),
	})
}

func (a *app) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/stats" {
		http.NotFound(w, r)
		return
	}
	t, err := template.ParseFS(webFS, "web/stats.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = t.Execute(w, map[string]string{
		"BasePath": a.cfg.BasePath,
	})
}

func (a *app) handleStatsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, a.stats())
}

func (a *app) handleInbox(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Local string `json:"local"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		local := strings.ToLower(strings.TrimSpace(req.Local))
		if local == "" {
			http.Error(w, "enter an address name", http.StatusBadRequest)
			return
		}
		if !localPartRE.MatchString(local) {
			http.Error(w, "invalid local part", http.StatusBadRequest)
			return
		}
		local = normalizeLocalPart(local)
		if !localPartRE.MatchString(local) {
			http.Error(w, "invalid local part", http.StatusBadRequest)
			return
		}
		address := local + "@" + a.domainForRequest(r)
		pass, err := randomPassword()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := a.createMaddyAccount(address, pass); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		now := time.Now()
		a.mu.Lock()
		a.inboxes[address] = &session{Address: address, Pass: pass, Created: now, Seen: now}
		a.mu.Unlock()
		writeJSON(w, map[string]string{"address": address})
	case http.MethodGet:
		address := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("address")))
		sess, ok := a.touch(address)
		if !ok {
			http.Error(w, "inbox not found", http.StatusNotFound)
			return
		}
		msgs, err := a.readMessages(address)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, inboxResponse{Address: sess.Address, Created: sess.Created, Seen: sess.Seen, Messages: msgs})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) domainForRequest(r *http.Request) string {
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(host, "www.")
	if a.cfg.Domains[host] {
		return host
	}
	return a.cfg.Domain
}

func (a *app) handleInboxItem(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/inbox/"), "/")
	if len(parts) < 1 {
		http.NotFound(w, r)
		return
	}
	addressBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}
	address := strings.ToLower(string(addressBytes))
	if r.Method == http.MethodDelete && len(parts) == 1 {
		a.deleteInbox(address)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet || len(parts) != 3 || parts[1] != "message" {
		http.NotFound(w, r)
		return
	}
	if _, ok := a.touch(address); !ok {
		http.NotFound(w, r)
		return
	}
	uid, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	raw, err := a.dumpMessage(address, uint32(uid))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%d.eml"`, safeFilename(address), uid))
	_, _ = w.Write(raw)
}

func (a *app) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := r.ParseMultipartForm(maxMessageBytes); err != nil {
		writeError(w, http.StatusBadRequest, "message is too large or the form could not be read")
		return
	}
	from := strings.ToLower(strings.TrimSpace(r.FormValue("from")))
	sess, ok := a.sessionForSender(from)
	if !ok {
		writeError(w, http.StatusBadRequest, "from address is not active and no all@ catch-all inbox is open for its domain")
		return
	}
	files := r.MultipartForm.File["attachments"]
	if total := totalAttachmentSize(files); total > maxAttachmentBytes {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("attachments are too large: %s selected, %s maximum", formatBytes(total), formatBytes(maxAttachmentBytes)))
		return
	}
	raw, err := buildOutbound(from, r.FormValue("to"), r.FormValue("subject"), r.FormValue("body"), files)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	envelopeFrom := from
	if strings.HasPrefix(sess.Address, "all@") && sess.Address != from {
		envelopeFrom = sess.Address
	}
	if err := a.submitMail(sess.Address, sess.Pass, envelopeFrom, splitRecipients(r.FormValue("to")), raw); err != nil {
		writeError(w, http.StatusFailedDependency, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "sent"})
}

func (a *app) sessionForSender(from string) (*session, bool) {
	_, domain, ok := strings.Cut(from, "@")
	if !ok || !a.cfg.Domains[domain] {
		return nil, false
	}
	if sess, ok := a.touch(from); ok {
		return sess, true
	}
	if normalized, ok := normalizeVariantAddress(from); ok && normalized != from {
		if sess, ok := a.touch(normalized); ok {
			return sess, true
		}
	}
	return a.touch("all@" + domain)
}

func (a *app) touch(address string) (*session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.inboxes[address]
	if !ok {
		return nil, false
	}
	sess.Seen = time.Now()
	cp := *sess
	return &cp, true
}

func (a *app) reaper() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-a.cfg.InboxTTL)
		var expired []string
		a.mu.Lock()
		for address, sess := range a.inboxes {
			if sess.Seen.Before(cutoff) {
				expired = append(expired, address)
				delete(a.inboxes, address)
			}
		}
		a.mu.Unlock()
		for _, address := range expired {
			a.removeMaddyAccount(address)
		}
	}
}

func (a *app) deleteInbox(address string) {
	a.mu.Lock()
	_, ok := a.inboxes[address]
	delete(a.inboxes, address)
	a.mu.Unlock()
	if ok {
		a.removeMaddyAccount(address)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func totalAttachmentSize(files []*multipart.FileHeader) int64 {
	var total int64
	for _, fh := range files {
		total += fh.Size
	}
	return total
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KiB", "MiB", "GiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB", value/unit)
}

func humanDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		hours := int(d / time.Hour)
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}
	if d%time.Minute == 0 {
		minutes := int(d / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	return d.String()
}

func (a *app) createMaddyAccount(address, pass string) error {
	if a.isProtectedAddress(address) {
		return a.openProtectedMaddyAccount(address, pass)
	}
	a.removeMaddyAccount(address)
	if out, err := a.runMaddy("", "creds", "create", "--password", pass, address); err != nil {
		return fmt.Errorf("create creds: %v: %s", err, out)
	}
	if out, err := a.runMaddy("", "imap-acct", "create", address); err != nil {
		_, _ = a.runMaddy("", "creds", "remove", "-y", address)
		return fmt.Errorf("create mailbox: %v: %s", err, out)
	}
	return nil
}

func (a *app) openProtectedMaddyAccount(address, pass string) error {
	if out, err := a.runMaddy("", "creds", "create", "--password", pass, address); err != nil {
		if !strings.Contains(out, "already exist") {
			return fmt.Errorf("create protected creds: %v: %s", err, out)
		}
		if out, err := a.runMaddy("", "creds", "password", "--password", pass, address); err != nil {
			return fmt.Errorf("rotate protected creds: %v: %s", err, out)
		}
	}
	if out, err := a.runMaddy("", "imap-acct", "create", address); err != nil && !strings.Contains(out, "user already exists") {
		return fmt.Errorf("create protected mailbox: %v: %s", err, out)
	}
	return nil
}

func (a *app) removeMaddyAccount(address string) {
	if a.isProtectedAddress(address) {
		log.Printf("preserving protected mailbox %s", address)
		return
	}
	if out, err := a.runMaddy("", "creds", "remove", "-y", address); err != nil {
		log.Printf("remove creds %s: %v: %s", address, err, out)
	}
	if out, err := a.runMaddy("", "imap-acct", "remove", "-y", address); err != nil {
		log.Printf("remove mailbox %s: %v: %s", address, err, out)
	}
}

func (a *app) removeStaleCatchAlls() {
	for _, domain := range sortedDomains(a.cfg.Domains) {
		address := "all@" + domain
		log.Printf("removing stale catch-all mailbox %s", address)
		a.removeMaddyAccount(address)
	}
}

func (a *app) isProtectedAddress(address string) bool {
	local, _, ok := strings.Cut(strings.ToLower(address), "@")
	if !ok {
		return true
	}
	return a.cfg.Protected[local]
}

func (a *app) stats() statsResponse {
	a.mu.Lock()
	inboxes := make([]inboxStat, 0, len(a.inboxes))
	for _, sess := range a.inboxes {
		inboxes = append(inboxes, inboxStat{
			Address: sess.Address,
			Created: sess.Created,
			Seen:    sess.Seen,
		})
	}
	a.mu.Unlock()

	sort.Slice(inboxes, func(i, j int) bool {
		return inboxes[i].Address < inboxes[j].Address
	})
	for i := range inboxes {
		count, err := a.messageCount(inboxes[i].Address)
		if err != nil {
			inboxes[i].Error = err.Error()
			continue
		}
		inboxes[i].MessageCount = count
	}
	return statsResponse{Generated: time.Now(), Inboxes: inboxes}
}

func (a *app) messageCount(address string) (int, error) {
	out, err := a.runMaddy("", "imap-msgs", "list", "-u", address, "INBOX")
	if err != nil {
		return 0, fmt.Errorf("list messages: %v: %s", err, out)
	}
	return len(parseUIDs(out)), nil
}

func (a *app) readMessages(address string) ([]message, error) {
	out, err := a.runMaddy("", "imap-msgs", "list", "-u", address, "INBOX")
	if err != nil {
		return nil, fmt.Errorf("list messages: %v: %s", err, out)
	}
	uids := parseUIDs(out)
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if len(uids) > 50 {
		uids = uids[:50]
	}
	msgs := make([]message, 0, len(uids))
	for _, uid := range uids {
		raw, err := a.dumpMessage(address, uid)
		if err != nil {
			log.Printf("dump message %s uid %d: %v", address, uid, err)
			continue
		}
		msgs = append(msgs, parseMessage(uid, raw))
	}
	return msgs, nil
}

func parseUIDs(out string) []uint32 {
	var uids []uint32
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "UID ") {
			continue
		}
		field := strings.TrimSuffix(strings.Fields(line)[1], ":")
		uid, err := strconv.ParseUint(field, 10, 32)
		if err == nil {
			uids = append(uids, uint32(uid))
		}
	}
	return uids
}

func (a *app) dumpMessage(address string, uid uint32) ([]byte, error) {
	out, err := a.runMaddy("", "imap-msgs", "dump", "-u", address, "INBOX", strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return nil, fmt.Errorf("dump message: %v: %s", err, out)
	}
	return []byte(stripDumpWarning(out)), nil
}

func stripDumpWarning(out string) string {
	if strings.HasPrefix(out, "WARNING:") {
		if idx := strings.Index(out, "\n"); idx >= 0 {
			return out[idx+1:]
		}
	}
	return out
}

func (a *app) runMaddy(stdin string, args ...string) (string, error) {
	return a.runMaddyWithInput(stdin, args...)
}

func (a *app) runMaddyWithInput(stdin string, args ...string) (string, error) {
	cmd := exec.Command(a.cfg.MaddyBin, args...)
	cmd.Dir = "/var/lib/maddy"
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func parseMessage(uid uint32, raw []byte) message {
	msg := message{ID: strconv.FormatUint(uint64(uid), 10), UID: uid, Date: time.Now()}
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		msg.Text = string(raw)
		return msg
	}
	msg.From = m.Header.Get("From")
	msg.DeliveredTo = m.Header.Get("Delivered-To")
	msg.Subject = m.Header.Get("Subject")
	if to := m.Header.Get("To"); to != "" {
		msg.To = splitRecipients(to)
	}
	if d, err := m.Header.Date(); err == nil {
		msg.Date = d
	}
	mediaType, params, _ := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" {
		readMultipart(&msg, multipart.NewReader(m.Body, params["boundary"]))
	} else {
		body, _ := io.ReadAll(io.LimitReader(m.Body, maxMessageBytes))
		if mediaType == "text/html" {
			msg.HTML = string(body)
		} else {
			msg.Text = string(body)
		}
	}
	return msg
}

func readMultipart(msg *message, mr *multipart.Reader) {
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			return
		}
		readPart(msg, part)
	}
}

func readPart(msg *message, part *multipart.Part) {
	defer part.Close()
	ct := part.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	disp, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	name := params["filename"]
	if name == "" {
		name = part.FileName()
	}
	data, _ := io.ReadAll(io.LimitReader(part, maxMessageBytes))
	size := int64(len(data))
	if strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "base64") {
		cleaned := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, data)
		if decoded, err := base64.StdEncoding.DecodeString(string(cleaned)); err == nil {
			size = int64(len(decoded))
		}
	}
	if disp == "attachment" || name != "" {
		msg.Attachments = append(msg.Attachments, attachment{Name: name, ContentType: ct, Size: size, Index: len(msg.Attachments)})
		return
	}
	mediaType, _, _ := mime.ParseMediaType(ct)
	switch mediaType {
	case "text/html":
		msg.HTML += string(data)
	default:
		msg.Text += string(data)
	}
}

func buildOutbound(from, to, subject, body string, files []*multipart.FileHeader) ([]byte, error) {
	recipients := splitRecipients(to)
	if len(recipients) == 0 {
		return nil, errors.New("missing recipient")
	}
	if !strings.Contains(from, "@") {
		return nil, errors.New("invalid sender")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	headers := textproto.MIMEHeader{}
	headers.Set("From", from)
	headers.Set("To", strings.Join(recipients, ", "))
	headers.Set("Subject", mime.QEncoding.Encode("utf-8", subject))
	headers.Set("Date", time.Now().Format(time.RFC1123Z))
	headers.Set("MIME-Version", "1.0")
	headers.Set("Content-Type", `multipart/mixed; boundary="`+w.Boundary()+`"`)
	for k, vals := range headers {
		for _, v := range vals {
			fmt.Fprintf(&buf, "%s: %s\r\n", k, v)
		}
	}
	buf.WriteString("\r\n")
	part, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Type":              {"text/plain; charset=utf-8"},
		"Content-Transfer-Encoding": {"8bit"},
	})
	if err != nil {
		return nil, err
	}
	_, _ = part.Write([]byte(body))
	for _, fh := range files {
		if err := addAttachment(w, fh); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func addAttachment(w *multipart.Writer, fh *multipart.FileHeader) error {
	f, err := fh.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	ct := fh.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(fh.Filename)))
	h.Set("Content-Transfer-Encoding", "base64")
	p, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	enc := base64.NewEncoder(base64.StdEncoding, newLineWriter{w: p})
	if _, err := io.Copy(enc, f); err != nil {
		return err
	}
	return enc.Close()
}

type newLineWriter struct{ w io.Writer }

func (n newLineWriter) Write(p []byte) (int, error) {
	consumed := len(p)
	for len(p) > 76 {
		if _, err := n.w.Write(p[:76]); err != nil {
			return consumed - len(p), err
		}
		if _, err := n.w.Write([]byte("\r\n")); err != nil {
			return consumed - len(p), err
		}
		p = p[76:]
	}
	if len(p) > 0 {
		if _, err := n.w.Write(p); err != nil {
			return consumed - len(p), err
		}
	}
	return consumed, nil
}

func (a *app) submitMail(user, pass, envelopeFrom string, recipients []string, raw []byte) error {
	host, _, err := net.SplitHostPort(a.cfg.SubmitAddr)
	if err != nil {
		return err
	}
	localSubmit := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if a.cfg.SubmitName != "" {
		host = a.cfg.SubmitName
	}
	c, err := smtp.Dial(a.cfg.SubmitAddr)
	if err != nil {
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		tlsConfig := &tls.Config{InsecureSkipVerify: localSubmit}
		if !localSubmit || a.cfg.SubmitName != "" {
			tlsConfig.ServerName = host
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if ok, _ := c.Extension("AUTH"); ok {
		if err := c.Auth(plainAuth{username: user, password: pass}); err != nil {
			return err
		}
	}
	if err := c.Mail(envelopeFrom); err != nil {
		return err
	}
	for _, rcpt := range recipients {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

type plainAuth struct {
	username string
	password string
}

func (a plainAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	resp := "\x00" + a.username + "\x00" + a.password
	return "PLAIN", []byte(resp), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected auth challenge")
	}
	return nil, nil
}

func splitRecipients(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\t' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if addr, err := mail.ParseAddress(f); err == nil {
			f = addr.Address
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func randomPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func safeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
	if s == "" {
		return "message"
	}
	return s
}
