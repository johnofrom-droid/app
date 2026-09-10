package main

// Render WhatsApp viewer.
//
// Important privacy behavior: this service never calls whatsmeow.MarkRead and
// never sends a read receipt. It is intentionally read-only for WhatsApp.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waHistory "go.mau.fi/whatsmeow/proto/waHistorySync"
	waWeb "go.mau.fi/whatsmeow/proto/waWeb"
	waStore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waTypes "go.mau.fi/whatsmeow/types"
	waEvents "go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const deviceName = "Meta Safety"

type message struct {
	ID       string `json:"id"`
	ChatID   string `json:"chatId"`
	Sender   string `json:"sender,omitempty"`
	Text     string `json:"text,omitempty"`
	Outgoing bool   `json:"outgoing"`
	Status   string `json:"status,omitempty"`
	Date     int64  `json:"date"`
}

type contact struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Number  string `json:"number,omitempty"`
	IsGroup bool   `json:"isGroup"`
}

type session struct {
	mu       sync.RWMutex
	status   string
	qr       string
	err      string
	client   *whatsmeow.Client
	messages []message
}

type app struct {
	mu       sync.RWMutex
	session  *session
	dataDir  string
	password string
	sessions map[string]string // token -> client IP
}

func main() {
	dataDir := getenv("DATA_DIR", ".data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	// This is the display/device label used when the account appears in
	// WhatsApp > Linked devices.
	waStore.DeviceProps.Os = proto.String(deviceName)
	waStore.BaseClientPayload.UserAgent.Device = proto.String(deviceName)

	a := &app{
		dataDir:  dataDir,
		password: getenv("APP_PASSWORD", "jos909@"),
		sessions: make(map[string]string),
	}
	// Always start the WhatsApp client. With no existing database, whatsmeow
	// creates an empty device store and exposes a QR code for first pairing.
	a.startWhatsApp()

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", a.ping)
	mux.HandleFunc("/login", a.login)
	mux.HandleFunc("/logout", a.logout)
	mux.HandleFunc("/", a.auth(a.index))
	mux.HandleFunc("/api/state", a.auth(a.state))
	mux.HandleFunc("/api/contacts", a.auth(a.contacts))
	mux.HandleFunc("/api/messages", a.auth(a.messages))
	mux.HandleFunc("/api/qr.png", a.auth(a.qrPNG))
	mux.HandleFunc("/api/events", a.auth(a.events))

	address := ":" + getenv("PORT", "10000")
	server := &http.Server{Addr: address, Handler: logging(mux)}
	log.Printf("%s listening on %s", deviceName, address)
	go func() {
		for {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("server error: %v; retrying", err)
				time.Sleep(2 * time.Second)
			}
		}
	}()
	select {}
}

func (a *app) ping(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "time": time.Now().UTC()})
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if a.authorized(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		header(w, http.StatusOK)
		_, _ = w.Write([]byte(loginHTML))
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.ParseForm() != nil || r.FormValue("password") != a.password {
		header(w, http.StatusUnauthorized)
		_, _ = w.Write([]byte(loginHTMLWithError))
		return
	}
	token := randomToken()
	a.mu.Lock()
	a.sessions[token] = clientIP(r)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "render_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 86400})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("render_session"); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "render_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) authorized(r *http.Request) bool {
	c, err := r.Cookie("render_session")
	if err != nil {
		return false
	}
	a.mu.RLock()
	ip, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	return ok && ip == clientIP(r)
}

func (a *app) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.authorized(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *app) index(w http.ResponseWriter, _ *http.Request) {
	header(w, http.StatusOK)
	_, _ = w.Write([]byte(indexHTML))
}

func (a *app) state(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	current := a.session
	a.mu.RUnlock()
	result := map[string]any{"deviceName": deviceName, "status": "not_started"}
	if current != nil {
		current.mu.RLock()
		result["status"], result["error"], result["hasQR"] = current.status, current.err, current.qr != ""
		current.mu.RUnlock()
	}
	writeJSON(w, result)
}

func (a *app) qrPNG(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	current := a.session
	a.mu.RUnlock()
	if current == nil {
		http.NotFound(w, nil)
		return
	}
	current.mu.RLock()
	qr := current.qr
	current.mu.RUnlock()
	if qr == "" {
		http.NotFound(w, nil)
		return
	}
	png, err := qrcode.Encode(qr, qrcode.Medium, 512)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(png)
}

func (a *app) contacts(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	current := a.session
	a.mu.RUnlock()
	if current == nil {
		writeJSON(w, []contact{})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	current.mu.RLock()
	client := current.client
	current.mu.RUnlock()
	if client == nil || client.Store == nil || client.Store.Contacts == nil {
		writeJSON(w, []contact{})
		return
	}
	all, err := client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	result := make([]contact, 0, len(all))
	known := make(map[string]bool, len(all))
	for jid, info := range all {
		name := strings.TrimSpace(info.FullName)
		if name == "" {
			name = strings.TrimSpace(info.PushName)
		}
		if name == "" {
			name = jid.User
		}
		if name == "" {
			name = "WhatsApp chat"
		}
		result = append(result, contact{ID: jid.String(), Name: name, Number: jid.User, IsGroup: jid.Server == waTypes.GroupServer})
		known[jid.String()] = true
	}
	// Some history-sync payloads contain a conversation that is not present in
	// the contacts store. Keep those chats visible instead of dropping them.
	current.mu.RLock()
	for _, item := range current.messages {
		if item.ChatID == "" || known[item.ChatID] {
			continue
		}
		known[item.ChatID] = true
		name := item.ChatID
		if jid, parseErr := waTypes.ParseJID(item.ChatID); parseErr == nil && jid.User != "" {
			name = jid.User
		}
		result = append(result, contact{ID: item.ChatID, Name: name, Number: name})
	}
	current.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name) })
	writeJSON(w, result)
}

func (a *app) messages(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	current := a.session
	a.mu.RUnlock()
	if current == nil {
		writeJSON(w, []message{})
		return
	}
	chat := r.URL.Query().Get("chat")
	current.mu.RLock()
	result := append([]message(nil), current.messages...)
	current.mu.RUnlock()
	if chat != "" {
		filtered := result[:0]
		for _, m := range result {
			if m.ChatID == chat {
				filtered = append(filtered, m)
			}
		}
		result = filtered
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Date < result[j].Date })
	writeJSON(w, result)
}

func (a *app) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			a.mu.RLock()
			current := a.session
			a.mu.RUnlock()
			status := "not_started"
			if current != nil {
				current.mu.RLock()
				status = current.status
				current.mu.RUnlock()
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonString(map[string]string{"status": status}))
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (a *app) startWhatsApp() {
	s := &session{status: "starting"}
	a.mu.Lock()
	a.session = s
	a.mu.Unlock()
	go a.runWhatsApp(s)
}

func (a *app) runWhatsApp(s *session) {
	ctx := context.Background()
	db := filepath.Join(a.dataDir, "whatsapp.db")
	container, err := sqlstore.New(ctx, "sqlite3", "file:"+db+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000", waLog.Stdout("WhatsAppStore", "WARN", false))
	if err != nil {
		a.fail(s, err)
		return
	}
	store, err := container.GetFirstDevice(ctx)
	if err != nil {
		a.fail(s, err)
		return
	}
	client := whatsmeow.NewClient(store, waLog.Stdout("WhatsApp", "WARN", false))
	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
	client.AddEventHandler(func(event any) { a.handleEvent(s, client, event) })
	if store.ID == nil {
		qrChan, err := client.GetQRChannel(ctx)
		if err != nil {
			a.fail(s, err)
			return
		}
		if err = client.Connect(); err != nil {
			a.fail(s, err)
			return
		}
		for qr := range qrChan {
			s.mu.Lock()
			if qr.Event == "code" {
				s.qr, s.status = qr.Code, "qr"
			}
			if qr.Event == "success" {
				s.qr, s.status = "", "connected"
			}
			if qr.Error != nil {
				s.err, s.status = qr.Error.Error(), "error"
			}
			s.mu.Unlock()
		}
	} else if err := client.Connect(); err != nil {
		a.fail(s, err)
		return
	} else {
		s.mu.Lock()
		s.status = "connected"
		s.mu.Unlock()
	}
	log.Printf("WhatsApp connected; read receipts are disabled by design")
	for {
		time.Sleep(25 * time.Second)
		if err := client.SendPresence(ctx, waTypes.PresenceAvailable); err != nil {
			log.Printf("presence keepalive: %v", err)
		}
	}
}

func (a *app) handleEvent(s *session, client *whatsmeow.Client, event any) {
	ctx := context.Background()
	if incoming, ok := event.(*waEvents.Message); ok && incoming.Message != nil {
		text := incoming.Message.GetConversation()
		if text == "" {
			text = incoming.Message.GetExtendedTextMessage().GetText()
		}
		if text != "" {
			a.append(s, message{ID: string(incoming.Info.ID), ChatID: incoming.Info.Chat.String(), Sender: incoming.Info.Sender.String(), Text: text, Outgoing: incoming.Info.IsFromMe, Status: func() string {
				if incoming.Info.IsFromMe {
					return "sent"
				}
				return ""
			}(), Date: incoming.Info.Timestamp.Unix()})
		}
	}
	if history, ok := event.(*waEvents.HistorySync); ok && history.Data != nil {
		a.history(s, client, history.Data, ctx)
	}
}

func (a *app) history(s *session, client *whatsmeow.Client, data *waHistory.HistorySync, ctx context.Context) {
	for _, conversation := range data.Conversations {
		for _, item := range conversation.Messages {
			if item == nil || item.Message == nil || item.Message.Key == nil || item.Message.Message == nil {
				continue
			}
			id, chat := item.Message.Key.GetID(), item.Message.Key.GetRemoteJID()
			if chat == "" {
				chat = conversation.GetID()
			}
			if id == "" || chat == "" {
				continue
			}
			text := item.Message.Message.GetConversation()
			if text == "" {
				text = item.Message.Message.GetExtendedTextMessage().GetText()
			}
			if text == "" {
				continue
			}
			status := ""
			if item.Message.Key.GetFromMe() {
				switch item.Message.GetStatus() {
				case waWeb.WebMessageInfo_READ, waWeb.WebMessageInfo_PLAYED:
					status = "read"
				case waWeb.WebMessageInfo_DELIVERY_ACK:
					status = "delivered"
				default:
					status = "sent"
				}
			}
			timestamp := int64(item.Message.GetMessageTimestamp())
			if timestamp == 0 {
				timestamp = int64(conversation.GetLastMsgTimestamp())
			}
			a.append(s, message{ID: id, ChatID: chat, Sender: item.Message.Key.GetParticipant(), Text: text, Outgoing: item.Message.Key.GetFromMe(), Status: status, Date: timestamp})
		}
	}
	s.mu.Lock()
	s.status = "connected"
	s.mu.Unlock()
	_ = ctx
}

func (a *app) append(s *session, m message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.messages {
		if old.ID == m.ID {
			return
		}
	}
	s.messages = append(s.messages, m)
}
func (a *app) fail(s *session, err error) {
	s.mu.Lock()
	s.status, s.err = "error", err.Error()
	s.mu.Unlock()
	log.Printf("WhatsApp: %v", err)
}
func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > -1 {
		return host[:i]
	}
	return host
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }
func header(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
}
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

var loginHTML = `<!doctype html><meta name="viewport" content="width=device-width"><title>Meta Safety</title><style>body{font:16px system-ui;max-width:420px;margin:15vh auto;padding:24px;background:#101827;color:#eef}input,button{width:100%;padding:12px;margin-top:10px;box-sizing:border-box}button{background:#25d366;border:0;color:#051;font-weight:700}</style><h1>Meta Safety</h1><p>Enter the server password.</p><form method="post"><input name="password" type="password" autofocus><button>Open</button></form>`
var loginHTMLWithError = `<!doctype html><meta name="viewport" content="width=device-width"><title>Meta Safety</title><style>body{font:16px system-ui;max-width:420px;margin:15vh auto;padding:24px;background:#101827;color:#eef}input,button{width:100%;padding:12px;margin-top:10px;box-sizing:border-box}button{background:#25d366;border:0;color:#051;font-weight:700}.e{color:#f88}</style><h1>Meta Safety</h1><p class="e">Wrong password.</p><form method="post"><input name="password" type="password" autofocus><button>Open</button></form>`
var indexHTML = `<!doctype html><meta name="viewport" content="width=device-width"><title>Meta Safety WhatsApp</title><style>body{font:15px system-ui;margin:0;background:#101827;color:#eef}header{padding:14px;background:#172338;display:flex;justify-content:space-between}main{display:grid;grid-template-columns:280px 1fr;min-height:calc(100vh - 55px)}aside{border-right:1px solid #293650;padding:12px;overflow:auto}.c{padding:10px;border-bottom:1px solid #293650;cursor:pointer;display:flex;gap:10px;align-items:center}.c:hover{background:#22324b}.avatar{width:32px;height:32px;border-radius:50%;background:#25d366;color:#062;display:inline-flex;align-items:center;justify-content:center;font-weight:700}#chat{padding:18px;white-space:pre-wrap;overflow:auto}.m{padding:8px;margin:5px 0;background:#1c2c44;border-radius:8px}.out{background:#174d3b}.qr{max-width:280px;background:white;padding:8px}button{padding:7px;background:#25d366;border:0;border-radius:5px}small{color:#9db0ca}@media(max-width:700px){main{grid-template-columns:1fr}aside{max-height:35vh;border-right:0;border-bottom:1px solid #293650}}</style><header><span>Meta Safety · WhatsApp viewer</span><a href="/logout"><button>Logout</button></a></header><main><aside><div id="state">Loading…</div><img id="qr" class="qr" hidden><h3>Contacts / chats</h3><div id="contacts"></div></aside><section id="chat"><h2>Select a chat</h2><p>This viewer does not mark messages as read.</p></section></main><script>let selected='';async function j(u){let r=await fetch(u);return r.json()}async function load(){let s=await j('/api/state');document.getElementById('state').innerHTML='<b>Status:</b> '+s.status+(s.error?'<br>'+s.error:'');let q=document.getElementById('qr');q.hidden=!s.hasQR;if(s.hasQR)q.src='/api/qr.png?'+Date.now();let cs=await j('/api/contacts');document.getElementById('contacts').innerHTML=cs.map(c=>'<div class="c" onclick="openChat(\''+encodeURIComponent(c.id)+'\')"><span class="avatar">'+esc((c.name||'?')[0].toUpperCase())+'</span><span><b>'+esc(c.name)+'</b><br><small>'+esc(c.number||c.id)+'</small></span></div>').join('')||'<small>Waiting for WhatsApp contacts/history…</small>';if(selected)showMessages()}async function openChat(id){selected=decodeURIComponent(id);showMessages()}async function showMessages(){let ms=await j('/api/messages?chat='+encodeURIComponent(selected));document.getElementById('chat').innerHTML='<h2>'+esc(selected)+'</h2>'+ms.map(m=>'<div class="m '+(m.outgoing?'out':'')+'"><small>'+new Date(m.date*1000).toLocaleString()+' · '+(m.status||'received')+'</small><br>'+esc(m.text)+'</div>').join('')||'<p>No cached text messages.</p>'}function esc(s){return String(s||'').replace(/[&<>\"']/g,x=>({'&':'&amp;','<':'&lt;','>':'&gt;','\"':'&quot;',"'":'&#39;'}[x]))}load();setInterval(load,5000)</script>`
