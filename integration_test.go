//go:build integration

package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"
)

const (
	integrationOperHash          = "$2a$04$ZgyWYPnm.ETL4Aq/RBLba.b.qb/Ky31EI7eA.yqoX7ksY5gfv00SC" // "operpass", test-only
	testDiscordMessageID         = "123456789012345678"
	testDiscordUserID            = "222222222222222222"
	testOutboundDiscordMessageID = "987654321098765432"
	testSelfDiscordMessageID     = "111111111111111111"
	testMultilineDiscordID       = "333333333333333333"
	testCaughtUpDiscordID        = "555555555555555555"
	testReplyDiscordID           = "666666666666666666"
)

type capturedReaction struct {
	replyID string
	emoji   string
	add     bool
}

type capturedOutbound struct {
	text string
	tags map[string]string
}

func TestErgoIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker unavailable")
	}

	const cdnBase = "https://cdn.discordapp.com/attachments/100/200/"
	photoSource := cdnBase + "photo.png?ex=abcdef&hm=" + strings.Repeat("a", 600)
	videoSource := cdnBase + "clip.mp4"
	failedSource := cdnBase + "rejected.png"
	attachments := []struct{ source, body, contentType, location string }{
		{photoSource, "\x89PNG\r\n\x1a\nphoto\x00payload", "image/png", "files/photo.png"},
		{videoSource, "\x00\x00\x00\x18ftypmp42\x00video payload", "video/mp4", "files/clip.mp4"},
		{failedSource, "rejected attachment bytes", "image/png", ""},
	}
	httpRequests := make(chan string, 16)
	releasePhoto := make(chan struct{})
	filehost := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpRequests <- r.Method + " " + r.URL.RequestURI()
		var body []byte
		if r.Method == http.MethodPost {
			user, password, ok := r.BasicAuth()
			if !ok || user != "Dickord" || password != "bridgepass" {
				t.Errorf("FILEHOST authentication: user=%q authenticated=%v", user, ok)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var err error
			body, err = io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading FILEHOST upload: %v", err)
				http.Error(w, "read failed", http.StatusBadRequest)
				return
			}
		}
		for _, attachment := range attachments {
			if r.Method == http.MethodGet && r.Host == "cdn.discordapp.com" && r.URL.RequestURI() == strings.TrimPrefix(attachment.source, "https://cdn.discordapp.com") {
				if r.Header.Get("Authorization") != "" {
					t.Error("FILEHOST credentials leaked to Discord CDN")
				}
				if attachment.source == photoSource {
					select {
					case <-releasePhoto:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", attachment.contentType)
				w.Header().Set("Content-Length", fmt.Sprint(len(attachment.body)))
				_, _ = io.WriteString(w, attachment.body)
				return
			}
			if r.Method == http.MethodPost && r.URL.Path == "/filehost/" && string(body) == attachment.body {
				if attachment.location == "" {
					http.Error(w, "upload rejected", http.StatusForbidden)
				} else {
					w.Header().Set("Location", attachment.location)
					w.WriteHeader(http.StatusCreated)
				}
				return
			}
		}
		t.Errorf("unexpected attachment HTTP request: %s %s body=%q", r.Method, r.URL, body)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer filehost.Close()

	temp := t.TempDir()
	certPool, port := startTestErgo(t, temp, filehost.URL+"/filehost/")
	registerTestAccount(t, port, certPool, "Dickord", "bridgepass")
	registerTestAccount(t, port, certPool, "owner", "ownerpass")
	registerTestAccount(t, port, certPool, "mallory", "mallorypass")

	fake := newFakeRDirCD(t)
	defer fake.Close()

	cfg := RuntimeConfig{
		Config: Config{
			Ergo: ErgoConfig{
				Address:       fmt.Sprintf("127.0.0.1:%d", port),
				TLSServerName: "ergo.test",
				Nick:          "Dickord",
				Account:       "Dickord",
				OperName:      "Dickord",
				OwnerAccounts: []string{"owner"},
			},
			RDirCD: RDirCDConfig{Address: fake.Address(), Nick: "dickord"},
			Channels: ChannelConfig{
				Prefix:           "discord.",
				DMIncludePattern: "me.*",
				CatchUp:          true,
				CatchUpLimit:     300,
				MaxNameLength:    64,
			},
			Reconnect: ReconnectConfig{Minimum: 100 * time.Millisecond, Maximum: time.Second},
		},
		ErgoPassword:     "bridgepass",
		OperPassword:     "operpass",
		ErgoRootCAs:      certPool,
		OwnerAccountsSet: map[string]struct{}{"owner": {}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	bridge := newBridge(cfg, logger)
	downloadTransport := filehost.Client().Transport.(*http.Transport).Clone()
	downloadTransport.TLSClientConfig.ServerName = filehost.Certificate().DNSNames[0]
	downloadTransport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "cdn.discordapp.com:443" {
			return nil, fmt.Errorf("unexpected CDN address %q", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, filehost.Listener.Addr().String())
	}
	defer downloadTransport.CloseIdleConnections()
	bridge.uploads.downloadClient.Transport = downloadTransport
	bridge.uploads.uploadClient.Transport = filehost.Client().Transport
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- bridge.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-bridgeDone:
			if err != nil {
				t.Errorf("bridge shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("bridge did not stop")
		}
	}()

	waitSignal(t, fake.accepted, "initial rdircd connection")
	waitSignal(t, fake.joined, "bridge joining fake rdircd channel")
	if got := waitText(t, fake.topics, "automatic watch command"); got != "log watch" {
		t.Fatalf("automatic watch args=%q", got)
	}

	owner, ownerMessages, ownerJoined := connectTestUser(t, port, certPool, "owner", "ownerpass", "#discord.me.chat.alice", false)
	defer owner.Quit()
	waitSignal(t, ownerJoined, "owner autojoin after connecting late")

	for _, state := range []string{"active", "paused", "done"} {
		if err := owner.SendWithTags(map[string]string{"+typing": state}, "TAGMSG", "#discord.me.chat.alice"); err != nil {
			t.Fatal(err)
		}
		if got := waitText(t, fake.typing, "owner typing "+state); got != state {
			t.Fatalf("typing state=%q, want %q", got, state)
		}
	}
	if err := owner.SendWithTags(map[string]string{"+typing": "invalid"}, "TAGMSG", "#discord.me.chat.alice"); err != nil {
		t.Fatal(err)
	}
	select {
	case state := <-fake.typing:
		t.Fatalf("malformed typing passed: %q", state)
	case <-time.After(200 * time.Millisecond):
	}

	fake.SendDiscord("hello from Discord")
	var ircMessageID string
	select {
	case msg := <-ownerMessages:
		if msg.Nick() != "Alice/discord" || msg.Params[1] != "hello from Discord" {
			t.Fatalf("unexpected relayed message: source=%q params=%q", msg.Nick(), msg.Params)
		}
		_, ircMessageID = msg.GetTag("msgid")
		if ircMessageID == "" {
			t.Fatal("relayed message has no Ergo msgid")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Discord-to-Ergo relay timed out")
	}

	expectDiscord := func(text string) ircmsg.Message {
		t.Helper()
		select {
		case msg := <-ownerMessages:
			if msg.Command != "PRIVMSG" || msg.Nick() != "Alice/discord" || len(msg.Params) < 2 || msg.Params[1] != text {
				t.Fatalf("relayed message=%+v, want Alice/discord PRIVMSG %q", msg, text)
			}
			if _, id := msg.GetTag("msgid"); id == "" {
				t.Fatal("attachment scenario relay has no Ergo msgid")
			}
			return msg
		case <-time.After(5 * time.Second):
			t.Fatalf("Discord relay timed out waiting for %q", text)
			return ircmsg.Message{}
		}
	}
	const photoDiscordID = "900000000000000001"
	const videoDiscordID = "900000000000000002"
	fake.SendDiscordID(photoDiscordID, "photo caption")
	fake.send("@+dickord/discord-msgid=" + photoDiscordID + ";+dickord/discord-userid=" + testDiscordUserID + ";+dickord/attachment=" + photoSource + " :Alice!Alice@discord PRIVMSG #me.chat.alice :[att] " + photoSource)
	fake.SendDiscordID("900000000000000003", "after photo")
	fake.send("@+dickord/discord-msgid=" + videoDiscordID + ";+dickord/discord-userid=" + testDiscordUserID + ";+dickord/discord-reply-msgid=" + testDiscordMessageID + ";+dickord/attachment=" + videoSource + " :Alice!Alice@discord PRIVMSG #me.chat.alice :[att] " + videoSource)
	fake.SendDiscordID("900000000000000004", "after video")
	caption := expectDiscord("photo caption")
	_, captionErgoID := caption.GetTag("msgid")
	if got, want := waitText(t, httpRequests, "signed photo download"), "GET "+strings.TrimPrefix(photoSource, "https://cdn.discordapp.com"); got != want {
		t.Fatalf("signed photo request=%q, want %q", got, want)
	}
	select {
	case msg := <-ownerMessages:
		t.Fatalf("message overtook pending attachment download: %+v", msg)
	case <-time.After(100 * time.Millisecond):
	}
	close(releasePhoto)
	photo := expectDiscord(filehost.URL + "/filehost/files/photo.png")
	_, photoErgoID := photo.GetTag("msgid")
	expectDiscord("after photo")
	video := expectDiscord(filehost.URL + "/filehost/files/clip.mp4")
	_, videoErgoID := video.GetTag("msgid")
	if _, reply := video.GetTag("+reply"); reply != ircMessageID {
		t.Fatalf("uploaded video lost native reply: got %q, want %q", reply, ircMessageID)
	}
	expectDiscord("after video")
	for _, want := range []string{"POST /filehost/", "GET /attachments/100/200/clip.mp4", "POST /filehost/"} {
		if got := waitText(t, httpRequests, "attachment HTTP request"); got != want {
			t.Fatalf("attachment request=%q, want %q", got, want)
		}
	}

	fake.SendDiscordReply("900000000000000005", videoDiscordID, "reply to uploaded video")
	videoReply := expectDiscord("reply to uploaded video")
	if _, reply := videoReply.GetTag("+reply"); reply != videoErgoID {
		t.Fatalf("reply to uploaded video=%q, want %q", reply, videoErgoID)
	}
	if err := owner.SendWithTags(map[string]string{"+reply": videoErgoID}, "PRIVMSG", "#discord.me.chat.alice", "native attachment reply"); err != nil {
		t.Fatal(err)
	}
	if got := waitOutbound(t, fake.outbound, "mapped attachment reply"); got.text != "native attachment reply" || got.tags["+dickord/discord-reply-msgid"] != videoDiscordID {
		t.Fatalf("mapped attachment reply=%+v", got)
	}
	fake.SendDelete(photoDiscordID)
	photoMessageIDs := map[string]bool{captionErgoID: true, photoErgoID: true}
	for range 2 {
		select {
		case msg := <-ownerMessages:
			if msg.Command != "REDACT" || len(msg.Params) < 2 || !photoMessageIDs[msg.Params[1]] {
				t.Fatalf("unexpected caption/attachment redaction: %+v", msg)
			}
			delete(photoMessageIDs, msg.Params[1])
		case <-time.After(3 * time.Second):
			t.Fatal("caption/attachment redaction timed out")
		}
	}

	fake.send("@+dickord/discord-msgid=900000000000000006;+dickord/discord-userid=" + testDiscordUserID + ";+dickord/attachment=" + failedSource + " :Alice!Alice@discord PRIVMSG #me.chat.alice :[att] " + failedSource)
	expectDiscord("[att] " + failedSource)
	for _, want := range []string{"GET /attachments/100/200/rejected.png", "POST /filehost/"} {
		if got := waitText(t, httpRequests, "rejected attachment HTTP request"); got != want {
			t.Fatalf("rejected attachment request=%q, want %q", got, want)
		}
	}
	fake.SendDiscordID("900000000000000007", videoSource)
	expectDiscord(videoSource)
	select {
	case request := <-httpRequests:
		t.Fatalf("untagged pasted URL caused attachment HTTP request: %q", request)
	case <-time.After(200 * time.Millisecond):
	}

	fake.SendDiscordReply(testReplyDiscordID, testDiscordMessageID, "Discord native reply")
	select {
	case msg := <-ownerMessages:
		_, reply := msg.GetTag("+reply")
		if msg.Params[1] != "Discord native reply" || reply != ircMessageID || strings.Contains(msg.Params[1], "-- re:") {
			t.Fatalf("Discord reply was not native: params=%q reply=%q", msg.Params, reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Discord native reply timed out")
	}

	fake.SendDiscordReply("777777777777777777", "888888888888888888", "unknown Discord reply")
	select {
	case msg := <-ownerMessages:
		_, reply := msg.GetTag("+reply")
		if msg.Params[1] != "unknown Discord reply" || reply != "" {
			t.Fatalf("unknown Discord reply fallback: params=%q reply=%q", msg.Params, reply)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unknown Discord reply fallback timed out")
	}

	if err := owner.SendWithTags(map[string]string{"+reply": ircMessageID}, "PRIVMSG", "#discord.me.chat.alice", "native reply"); err != nil {
		t.Fatal(err)
	}
	if got := waitOutbound(t, fake.outbound, "mapped native reply"); got.text != "native reply" || got.tags["+dickord/discord-reply-msgid"] != testDiscordMessageID {
		t.Fatalf("mapped reply=%+v", got)
	}

	if err := owner.Privmsg("#discord.me.chat.alice", "Alice/discord: hi"); err != nil {
		t.Fatal(err)
	}
	if got := waitOutbound(t, fake.outbound, "cached Discord mention"); got.text != "<@"+testDiscordUserID+"> hi" {
		t.Fatalf("cached mention=%+v", got)
	}
	if err := owner.Privmsg("#discord.me.chat.alice", "notice how @Alice looked"); err != nil {
		t.Fatal(err)
	}
	if got := waitOutbound(t, fake.outbound, "cached inline Discord mention"); got.text != "notice how <@"+testDiscordUserID+"> looked" {
		t.Fatalf("cached inline mention=%+v", got)
	}

	if err := owner.SendWithTags(map[string]string{"+reply": "unknown-ergo-msgid"}, "PRIVMSG", "#discord.me.chat.alice", "unknown reply target"); err != nil {
		t.Fatal(err)
	}
	if got := waitOutbound(t, fake.outbound, "unknown reply fallback"); got.text != "unknown reply target" || got.tags["+dickord/discord-reply-msgid"] != "" {
		t.Fatalf("unknown reply fallback=%+v", got)
	}

	const crossChannelErgoID = "cross-channel-ergo-msgid"
	bridge.mu.Lock()
	bridge.discordRefs[crossChannelErgoID] = discordMessageRef{source: "#other.channel", messageID: testDiscordMessageID}
	bridge.mu.Unlock()
	if err := owner.SendWithTags(map[string]string{"+reply": crossChannelErgoID}, "PRIVMSG", "#discord.me.chat.alice", "cross-channel reply target"); err != nil {
		t.Fatal(err)
	}
	if got := waitOutbound(t, fake.outbound, "cross-channel reply fallback"); got.text != "cross-channel reply target" || got.tags["+dickord/discord-reply-msgid"] != "" {
		t.Fatalf("cross-channel reply fallback=%+v", got)
	}

	longReply := strings.Repeat("split reply text ", 23)
	parts := splitUTF8(longReply, 380)
	if len(parts) < 2 {
		t.Fatal("split reply fixture did not split")
	}
	if err := owner.SendWithTags(map[string]string{"+reply": ircMessageID}, "PRIVMSG", "#discord.me.chat.alice", longReply); err != nil {
		t.Fatal(err)
	}
	for index, want := range parts {
		got := waitOutbound(t, fake.outbound, "split reply chunk")
		if got.text != want {
			t.Fatalf("split reply chunk %d text=%q, want %q", index, got.text, want)
		}
		wantReply := ""
		if index == 0 {
			wantReply = testDiscordMessageID
		}
		if got.tags["+dickord/discord-reply-msgid"] != wantReply {
			t.Fatalf("split reply chunk %d reference=%q, want %q", index, got.tags["+dickord/discord-reply-msgid"], wantReply)
		}
	}

	fake.SendSelfDiscord("DiscordDisplayName", "hello from official Discord")
	select {
	case msg := <-ownerMessages:
		if msg.Nick() != "owner/discord" || msg.Params[1] != "hello from official Discord" {
			t.Fatalf("self-authored Discord message was not mapped to owner: source=%q params=%q", msg.Nick(), msg.Params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("self-authored Discord message relay timed out")
	}

	fake.SendReaction(testDiscordMessageID, "Bob", "+👍", "Alice", "hello from Discord")
	select {
	case msg := <-ownerMessages:
		_, reply := msg.GetTag("+reply")
		_, reaction := msg.GetTag("+react")
		if msg.Command != "TAGMSG" || msg.Nick() != "Dickord" || reply != ircMessageID || reaction != "👍" {
			t.Fatalf("unexpected native reaction: command=%q source=%q reply=%q reaction=%q", msg.Command, msg.Nick(), reply, reaction)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Discord reaction-to-IRC translation timed out")
	}
	fake.SendReaction(testDiscordMessageID, "Bob", "-👍", "Alice", "hello from Discord")
	select {
	case msg := <-ownerMessages:
		_, reply := msg.GetTag("+reply")
		_, reaction := msg.GetTag("+unreact")
		if msg.Command != "TAGMSG" || reply != ircMessageID || reaction != "👍" {
			t.Fatalf("unexpected native unreaction: command=%q reply=%q reaction=%q", msg.Command, reply, reaction)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Discord unreaction-to-IRC translation timed out")
	}

	bridge.relayReactionWithRetry(
		"#discord.me.chat.alice", "Bob", discordReaction{add: true, emoji: "❓"},
		"--- reacts: +❓", false, 0, "#me.chat.alice", "444444444444444444",
	)
	select {
	case msg := <-ownerMessages:
		t.Fatalf("uncorrelated reaction leaked as text: %+v", msg)
	case <-time.After(300 * time.Millisecond):
	}

	fake.SendUnsupportedReaction(testDiscordMessageID, "Bob", "react-remove-all", "-all")
	select {
	case msg := <-ownerMessages:
		if len(msg.Params) < 2 || !strings.Contains(msg.Params[1], "reacts: -all") {
			t.Fatalf("unsupported reaction notice=%+v", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unsupported reaction notice timed out")
	}

	fake.SendLegacyReaction(testDiscordMessageID, "Bob", "+✅", "Alice", "hello from Discord")
	select {
	case msg := <-ownerMessages:
		_, reaction := msg.GetTag("+react")
		if msg.Command != "TAGMSG" || reaction != "✅" {
			t.Fatalf("legacy reaction was not translated natively: %+v", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("legacy reaction translation timed out")
	}

	if err := owner.SendWithTags(map[string]string{"+reply": ircMessageID, "+draft/react": "🔥"}, "PRIVMSG", "#discord.me.chat.alice", "🔥"); err != nil {
		t.Fatal(err)
	}
	select {
	case reaction := <-fake.reactions:
		if reaction.replyID != testDiscordMessageID || reaction.emoji != "🔥" || !reaction.add {
			t.Fatalf("unexpected IRC-to-Discord reaction: %+v", reaction)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IRC reaction-to-Discord translation timed out")
	}
	select {
	case got := <-fake.outbound:
		t.Fatalf("reaction body leaked as Discord message: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}
	fake.SendReaction(testDiscordMessageID, "owner", "+🔥", "Alice", "hello from Discord")
	select {
	case msg := <-ownerMessages:
		t.Fatalf("Discord reaction confirmation was duplicated onto IRC: %+v", msg)
	case <-time.After(300 * time.Millisecond):
	}

	if err := owner.SendWithTags(map[string]string{"+reply": ircMessageID, "+draft/unreact": ":Party:"}, "TAGMSG", "#discord.me.chat.alice"); err != nil {
		t.Fatal(err)
	}
	select {
	case reaction := <-fake.reactions:
		if reaction.replyID != testDiscordMessageID || reaction.emoji != ":Party:" || reaction.add {
			t.Fatalf("unexpected custom IRC-to-Discord unreaction: %+v", reaction)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("custom IRC unreaction-to-Discord translation timed out")
	}
	fake.SendReaction(testDiscordMessageID, "owner", "-:Party:", "Alice", "hello from Discord")
	select {
	case msg := <-ownerMessages:
		t.Fatalf("custom Discord unreaction confirmation was duplicated onto IRC: %+v", msg)
	case <-time.After(300 * time.Millisecond):
	}

	const delayedErgoID = "delayed-ergo-msgid"
	delayed := ircmsg.MakeMessage(map[string]string{"account": "owner", "+reply": delayedErgoID, "+draft/react": "👀"}, "owner!u@h", "TAGMSG", "#discord.me.chat.alice")
	bridge.mu.RLock()
	ergoConn := bridge.ergo
	bridge.mu.RUnlock()
	bridge.onErgoReaction(ergoConn, delayed)
	bridge.onErgoReaction(ergoConn, delayed)
	time.AfterFunc(250*time.Millisecond, func() {
		bridge.mu.Lock()
		bridge.discordRefs[delayedErgoID] = discordMessageRef{source: "#me.chat.alice", messageID: testDiscordMessageID}
		bridge.mu.Unlock()
	})
	select {
	case reaction := <-fake.reactions:
		if reaction.emoji != "👀" || !reaction.add {
			t.Fatalf("unexpected delayed reaction: %+v", reaction)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reaction did not wait for delayed message ID mapping")
	}
	select {
	case reaction := <-fake.reactions:
		t.Fatalf("duplicate pending reaction was forwarded: %+v", reaction)
	case <-time.After(300 * time.Millisecond):
	}
	fake.SendDelete(testDiscordMessageID)
	select {
	case msg := <-ownerMessages:
		if msg.Command != "REDACT" || len(msg.Params) < 2 || msg.Params[1] != ircMessageID {
			t.Fatalf("unexpected native redaction: %+v", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Discord deletion-to-IRC REDACT timed out")
	}

	fake.SendDiscordID(testCaughtUpDiscordID, "[2026-08-27 00:00:00] caught up")
	var caughtUpErgoID string
	select {
	case msg := <-ownerMessages:
		_, caughtUpErgoID = msg.GetTag("msgid")
	case <-time.After(3 * time.Second):
		t.Fatal("caught-up Discord relay timed out")
	}
	fake.SendReaction(testCaughtUpDiscordID, "Bob", "+📌", "Alice", "caught up")
	select {
	case msg := <-ownerMessages:
		_, reply := msg.GetTag("+reply")
		if msg.Command != "TAGMSG" || reply != caughtUpErgoID {
			t.Fatalf("caught-up reaction target=%q, want %q", reply, caughtUpErgoID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("caught-up reaction translation timed out")
	}

	fake.SendDiscordID(testMultilineDiscordID, "first line", "second line")
	multilineIDs := make(map[string]bool)
	for range 2 {
		select {
		case msg := <-ownerMessages:
			_, id := msg.GetTag("msgid")
			if id == "" {
				t.Fatal("multiline relay has no Ergo msgid")
			}
			multilineIDs[id] = true
		case <-time.After(3 * time.Second):
			t.Fatal("multiline Discord relay timed out")
		}
	}
	fake.SendDelete(testMultilineDiscordID)
	for range 2 {
		select {
		case msg := <-ownerMessages:
			if msg.Command != "REDACT" || len(msg.Params) < 2 || !multilineIDs[msg.Params[1]] {
				t.Fatalf("unexpected multiline redaction: %+v", msg)
			}
			delete(multilineIDs, msg.Params[1])
		case <-time.After(3 * time.Second):
			t.Fatal("multiline Discord redaction timed out")
		}
	}

	voiceURL := filehost.URL + "/files/voice-message.ogg"
	voiceTags := map[string]string{
		"+dickord/voice-duration": "2.5",
		"+dickord/voice-waveform": "AAE=",
	}
	if err := owner.Privmsg("#discord.me.chat.alice", voiceURL); err != nil {
		t.Fatal(err)
	}
	if got := waitOutbound(t, fake.outbound, "FILEHOST upload relay"); got.text != voiceURL ||
		got.tags["+dickord/upload"] != "1" {
		t.Fatalf("voice outbound=%+v", got)
	}

	if err := owner.SendWithTags(voiceTags, "PRIVMSG", "#discord.me.chat.alice", "https://example.com/voice-message.ogg"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-ownerMessages:
		if len(msg.Params) < 2 || !strings.Contains(msg.Params[1], "Invalid voice message") {
			t.Fatalf("unexpected invalid voice response: %+v", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("invalid voice response timed out")
	}
	select {
	case got := <-fake.outbound:
		t.Fatalf("invalid voice message passed: %+v", got)
	case <-time.After(200 * time.Millisecond):
	}

	if err := owner.Privmsg("#discord.me.chat.alice", "hello from Ergo"); err != nil {
		t.Fatal(err)
	}
	if got := waitOutbound(t, fake.outbound, "Ergo-to-Discord relay"); got.text != "hello from Ergo" {
		t.Fatalf("outbound=%+v", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	var outboundErgoID string
	for {
		bridge.mu.RLock()
		for ergoID, ref := range bridge.discordRefs {
			if ref.messageID == testOutboundDiscordMessageID {
				outboundErgoID = ergoID
				break
			}
		}
		bridge.mu.RUnlock()
		if outboundErgoID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rdircd delivery acknowledgement was not cached")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := owner.SendWithTags(map[string]string{"+reply": outboundErgoID, "+react": "🧪"}, "TAGMSG", "#discord.me.chat.alice"); err != nil {
		t.Fatal(err)
	}
	select {
	case reaction := <-fake.reactions:
		if reaction.replyID != testOutboundDiscordMessageID || reaction.emoji != "🧪" || !reaction.add {
			t.Fatalf("unexpected IRC-origin reaction: %+v", reaction)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("IRC-origin reaction was not forwarded")
	}
	fake.SendReaction(testOutboundDiscordMessageID, "owner", "+🧪", "", "")
	select {
	case msg := <-ownerMessages:
		t.Fatalf("unreferenced own-reaction confirmation leaked to IRC: %+v", msg)
	case <-time.After(300 * time.Millisecond):
	}
	if err := owner.Send("REDACT", "#discord.me.chat.alice", outboundErgoID, "test cleanup"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-ownerMessages:
		if msg.Command != "REDACT" {
			t.Fatalf("unexpected message after local REDACT: %+v", msg)
		}
	case <-time.After(100 * time.Millisecond):
	}
	if got := waitText(t, fake.redactions, "IRC REDACT-to-Discord deletion"); got != testOutboundDiscordMessageID {
		t.Fatalf("redaction Discord ID=%q", got)
	}
	fake.SendDelete(testOutboundDiscordMessageID)
	select {
	case msg := <-ownerMessages:
		t.Fatalf("Discord deletion confirmation leaked to IRC: %+v", msg)
	case <-time.After(300 * time.Millisecond):
	}

	if err := owner.Privmsg("#discord.me.chat.alice", "!topic log 2h"); err != nil {
		t.Fatal(err)
	}
	if got := waitText(t, fake.topics, "topic translation"); got != "log 2h" {
		t.Fatalf("topic args=%q", got)
	}

	mallory, _, _ := connectTestUser(t, port, certPool, "mallory", "mallorypass", "#discord.me.chat.alice", true)
	defer mallory.Quit()
	if err := mallory.Privmsg("#discord.me.chat.alice", "must not pass"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fake.outbound:
		t.Fatalf("unauthorized message passed: %+v", got)
	case <-time.After(500 * time.Millisecond):
	}
	if err := mallory.SendWithTags(map[string]string{"+reply": ircMessageID}, "PRIVMSG", "#discord.me.chat.alice", "unauthorized reply"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fake.outbound:
		t.Fatalf("unauthorized reply passed: %+v", got)
	case <-time.After(500 * time.Millisecond):
	}
	if err := mallory.SendWithTags(map[string]string{"+typing": "active"}, "TAGMSG", "#discord.me.chat.alice"); err != nil {
		t.Fatal(err)
	}
	select {
	case state := <-fake.typing:
		t.Fatalf("unauthorized typing passed: %q", state)
	case <-time.After(500 * time.Millisecond):
	}

	runDocker(t, "stop", testErgoName(t))
	waitSignal(t, fake.disconnected, "rdircd disconnect after Ergo loss")
	runDocker(t, "start", testErgoName(t))
	waitSignal(t, fake.accepted, "rdircd reconnect after Ergo recovery")
}

func startTestErgo(t *testing.T, dir, filehostURL string) (*x509.CertPool, int) {
	t.Helper()
	port := freePort(t)
	certPEM, keyPEM := testCertificate(t)
	writeTestFile(t, filepath.Join(dir, "fullchain.pem"), certPEM, 0o600)
	writeTestFile(t, filepath.Join(dir, "privkey.pem"), keyPEM, 0o600)
	config := fmt.Sprintf(`
network:
    name: DickordTest
server:
    name: ergo.test
    additional-isupport:
        "soju.im/FILEHOST": %q
    listeners:
        ":%d":
            tls:
                cert: fullchain.pem
                key: privkey.pem
            min-tls-version: 1.2
    sts:
        enabled: false
    casemapping: precis
    enforce-utf8: true
    lookup-hostnames: false
    max-sendq: 96k
    ip-limits:
        count: false
        throttle: false
    relaymsg:
        enabled: true
        separators: "/"
        available-to-chanops: true
accounts:
    authentication-enabled: true
    registration:
        enabled: true
        allow-before-connect: true
        email-verification:
            enabled: false
        bcrypt-cost: 4
        throttling:
            enabled: false
    login-throttling:
        enabled: false
    require-sasl:
        enabled: false
    nick-reservation:
        enabled: false
    multiclient:
        enabled: true
        allowed-by-default: true
channels:
    default-modes: +nt
    auto-join: ["#general"]
    registration:
        enabled: true
history:
    enabled: true
    channel-length: 128
    client-length: 64
    autoreplay-on-join: 0
    chathistory-maxmessages: 1000
    persistent:
        enabled: true
        unregistered-channels: true
        registered-channels: "mandatory"
        direct-messages: "mandatory"
    retention:
        allow-individual-delete: true
datastore:
    path: /ircd/ircd.db
    autoupgrade: true
    sqlite:
        enabled: true
        database-path: /ircd/history.db
languages:
    enabled: true
    default: en
    path: /ircd-bin/languages
limits:
    nicklen: 32
    identlen: 20
    channellen: 64
    awaylen: 390
    kicklen: 390
    topiclen: 390
    monitor-entries: 100
    whowas-entries: 100
    chan-list-modes: 60
    registration-messages: 1024
oper-classes:
    relay-bridge:
        title: Relay Bridge
        capabilities: ["history", "relaymsg", "sajoin"]
opers:
    Dickord:
        class: relay-bridge
        hidden: true
        password: %q
logging:
    -
        method: stderr
        type: "* -userinput -useroutput"
        level: info
`, filehostURL, port, integrationOperHash)
	writeTestFile(t, filepath.Join(dir, "ircd.yaml"), []byte(config), 0o600)

	name := testErgoName(t)
	runDocker(t, "rm", "-f", name)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	runDocker(t, "run", "-d", "--name", name, "--network", "host", "-v", dir+":/ircd", "ghcr.io/ergochat/ergo:v2.19.1")

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to load test CA")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{RootCAs: pool, ServerName: "ergo.test"})
		if err == nil {
			conn.Close()
			return pool, port
		}
		time.Sleep(100 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
	t.Fatalf("Ergo did not start:\n%s", logs)
	return nil, 0
}

func registerTestAccount(t *testing.T, port int, roots *x509.CertPool, account, password string) {
	t.Helper()
	registered := make(chan struct{}, 1)
	conn := &ircevent.Connection{
		Server:    fmt.Sprintf("127.0.0.1:%d", port),
		Nick:      account,
		User:      account,
		UseTLS:    true,
		TLSConfig: &tls.Config{RootCAs: roots, ServerName: "ergo.test"},
		Timeout:   5 * time.Second,
		Log:       silentLogger(),
	}
	conn.AddConnectCallback(func(ircmsg.Message) {
		_ = conn.Send("NICKSERV", "REGISTER", password, "*")
	})
	conn.AddCallback("NOTICE", func(msg ircmsg.Message) {
		if len(msg.Params) > 1 && (strings.Contains(strings.ToLower(msg.Params[1]), "successfully registered") || strings.Contains(strings.ToLower(msg.Params[1]), "account created")) {
			select {
			case registered <- struct{}{}:
			default:
			}
		}
	})
	if err := conn.Connect(); err != nil {
		t.Fatalf("connect registration client %s: %v", account, err)
	}
	go conn.Loop()
	waitSignal(t, registered, "register account "+account)
	conn.Quit()
}

func connectTestUser(t *testing.T, port int, roots *x509.CertPool, account, password, channel string, joinOnConnect bool) (*ircevent.Connection, <-chan ircmsg.Message, <-chan struct{}) {
	t.Helper()
	joined := make(chan struct{}, 1)
	messages := make(chan ircmsg.Message, 8)
	conn := &ircevent.Connection{
		Server:       fmt.Sprintf("127.0.0.1:%d", port),
		Nick:         account,
		User:         account,
		UseTLS:       true,
		TLSConfig:    &tls.Config{RootCAs: roots, ServerName: "ergo.test"},
		SASLLogin:    account,
		SASLPassword: password,
		RequestCaps:  []string{"account-tag", "message-tags", "draft/message-redaction", "draft/relaymsg"},
		Timeout:      5 * time.Second,
		Log:          silentLogger(),
	}
	if joinOnConnect {
		conn.AddConnectCallback(func(ircmsg.Message) { _ = conn.Join(channel) })
	}
	conn.AddCallback("JOIN", func(msg ircmsg.Message) {
		if msg.Nick() == conn.CurrentNick() && len(msg.Params) > 0 && ircCasefold(msg.Params[0]) == ircCasefold(channel) {
			select {
			case joined <- struct{}{}:
			default:
			}
		}
	})
	handleMessage := func(msg ircmsg.Message) {
		if len(msg.Params) > 0 && ircCasefold(msg.Params[0]) == ircCasefold(channel) {
			messages <- msg
		}
	}
	conn.AddCallback("PRIVMSG", handleMessage)
	conn.AddCallback("TAGMSG", handleMessage)
	conn.AddCallback("REDACT", handleMessage)
	if err := conn.Connect(); err != nil {
		t.Fatalf("connect %s: %v", account, err)
	}
	go conn.Loop()
	if joinOnConnect {
		waitSignal(t, joined, account+" joining "+channel)
	}
	return conn, messages, joined
}

type fakeRDirCD struct {
	t            *testing.T
	listener     net.Listener
	mu           sync.Mutex
	current      net.Conn
	writer       *bufio.Writer
	accepted     chan struct{}
	joined       chan struct{}
	disconnected chan struct{}
	outbound     chan capturedOutbound
	typing       chan string
	topics       chan string
	reactions    chan capturedReaction
	redactions   chan string
	closed       chan struct{}
}

func newFakeRDirCD(t *testing.T) *fakeRDirCD {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRDirCD{
		t:            t,
		listener:     listener,
		accepted:     make(chan struct{}, 8),
		joined:       make(chan struct{}, 8),
		disconnected: make(chan struct{}, 8),
		outbound:     make(chan capturedOutbound, 16),
		typing:       make(chan string, 8),
		topics:       make(chan string, 8),
		reactions:    make(chan capturedReaction, 8),
		redactions:   make(chan string, 8),
		closed:       make(chan struct{}),
	}
	go f.acceptLoop()
	return f
}

func (f *fakeRDirCD) Address() string { return f.listener.Addr().String() }

func (f *fakeRDirCD) Close() {
	close(f.closed)
	_ = f.listener.Close()
	f.mu.Lock()
	if f.current != nil {
		_ = f.current.Close()
	}
	f.mu.Unlock()
}

func (f *fakeRDirCD) SendDiscord(text string) {
	f.SendDiscordID(testDiscordMessageID, text)
}

func (f *fakeRDirCD) SendDiscordID(messageID string, lines ...string) {
	for _, text := range lines {
		f.send("@+dickord/discord-msgid=" + messageID + ";+dickord/discord-userid=" + testDiscordUserID + " :Alice!Alice@discord PRIVMSG #me.chat.alice :" + text)
	}
}

func (f *fakeRDirCD) SendDiscordReply(messageID, replyMessageID, text string) {
	f.send("@+dickord/discord-msgid=" + messageID + ";+dickord/discord-userid=" + testDiscordUserID + ";+dickord/discord-reply-msgid=" + replyMessageID + " :Alice!Alice@discord PRIVMSG #me.chat.alice :" + text)
}

func (f *fakeRDirCD) SendSelfDiscord(nick, text string) {
	f.send("@+dickord/discord-msgid=" + testSelfDiscordMessageID + ";+dickord/discord-userid=" + testDiscordUserID + ";+dickord/self=1 :" + nick + "!self@discord PRIVMSG #me.chat.alice :" + text)
}

func (f *fakeRDirCD) SendDelete(discordMessageID string) {
	f.send("@+dickord/discord-msgid=" + discordMessageID + ";+dickord/event=delete :core!core@discord NOTICE #me.chat.alice :--- message was deleted [2026-08-27T00:00:00.000Z]")
}

func (f *fakeRDirCD) SendUnsupportedReaction(discordMessageID, reactor, event, reaction string) {
	f.send("@+dickord/discord-msgid=" + discordMessageID + ";+dickord/discord-userid=" + testDiscordUserID + ";+dickord/event=" + event + " :" + reactor + "!" + reactor + "@discord PRIVMSG #me.chat.alice :--- reacts: " + reaction)
}

func (f *fakeRDirCD) SendLegacyReaction(discordMessageID, reactor, reaction, originalNick, originalText string) {
	text := "--- reacts: " + reaction + " :: [2026-08-27T00:00:00.000Z] <" + originalNick + "> " + originalText
	f.send("@+dickord/discord-msgid=" + discordMessageID + " :" + reactor + "!" + reactor + "@discord PRIVMSG #me.chat.alice :" + text)
}

func (f *fakeRDirCD) SendReaction(discordMessageID, reactor, reaction, originalNick, originalText string) {
	text := "--- reacts: " + reaction + " [2026-08-27T00:00:00.000Z]"
	if originalNick != "" {
		text = "--- reacts: " + reaction + " :: [2026-08-27T00:00:00.000Z] <" + originalNick + "> " + originalText
	}
	event := "react-add"
	if strings.HasPrefix(reaction, "-") {
		event = "react-remove"
	}
	emoji := strings.TrimLeft(reaction, "+-")
	f.send("@+dickord/discord-msgid=" + discordMessageID + ";+dickord/discord-userid=" + testDiscordUserID + ";+dickord/event=" + event + ";+dickord/emoji=" + emoji + ";+dickord/discord-emoji=" + emoji + " :" + reactor + "!" + reactor + "@discord PRIVMSG #me.chat.alice :" + text)
}

func (f *fakeRDirCD) acceptLoop() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		select {
		case f.accepted <- struct{}{}:
		default:
		}
		go f.handle(conn)
	}
}

func (f *fakeRDirCD) handle(conn net.Conn) {
	f.mu.Lock()
	f.current = conn
	f.writer = bufio.NewWriter(conn)
	f.mu.Unlock()
	defer func() {
		_ = conn.Close()
		f.mu.Lock()
		if f.current == conn {
			f.current, f.writer = nil, nil
		}
		f.mu.Unlock()
		select {
		case f.disconnected <- struct{}{}:
		default:
		}
	}()

	scanner := bufio.NewScanner(conn)
	nick, user, capDone, welcomed := "dickord", false, false, false
	welcome := func() {
		if welcomed || !user || !capDone {
			return
		}
		welcomed = true
		f.sendTo(conn, fmt.Sprintf(":fake 001 %s :welcome", nick))
		f.sendTo(conn, fmt.Sprintf(":fake 422 %s :no motd", nick))
	}
	for scanner.Scan() {
		line := scanner.Text()
		tags := make(map[string]string)
		if strings.HasPrefix(line, "@") {
			tagText, command, found := strings.Cut(line, " ")
			if !found {
				continue
			}
			for _, tag := range strings.Split(strings.TrimPrefix(tagText, "@"), ";") {
				key, value, _ := strings.Cut(tag, "=")
				tags[key] = value
			}
			if strings.HasPrefix(strings.ToUpper(command), "TAGMSG #ME.CHAT.ALICE") {
				if state := tags["+typing"]; state != "" {
					f.typing <- state
					continue
				}
				emoji, add := tags["+draft/react"], true
				if emoji == "" {
					emoji, add = tags["+draft/unreact"], false
				}
				f.reactions <- capturedReaction{replyID: tags["+reply"], emoji: emoji, add: add}
				continue
			}
			line = command
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "CAP LS"):
			f.sendTo(conn, ":fake CAP * LS :message-tags")
		case strings.HasPrefix(upper, "CAP REQ"):
			capabilities := line[strings.LastIndex(line, ":")+1:]
			f.sendTo(conn, ":fake CAP * ACK :"+capabilities)
		case strings.HasPrefix(upper, "CAP END"):
			capDone = true
			welcome()
		case strings.HasPrefix(upper, "NICK "):
			nick = strings.TrimSpace(line[5:])
		case strings.HasPrefix(upper, "USER "):
			user = true
			welcome()
		case upper == "LIST":
			f.sendTo(conn, fmt.Sprintf(":fake 322 %s #me.chat.alice 1 :DM", nick))
			f.sendTo(conn, fmt.Sprintf(":fake 323 %s :end", nick))
		case strings.HasPrefix(upper, "JOIN "):
			channel := strings.TrimSpace(line[5:])
			f.sendTo(conn, fmt.Sprintf(":%s!u@fake JOIN %s", nick, channel))
			f.sendTo(conn, fmt.Sprintf(":fake 366 %s %s :end", nick, channel))
			if ircCasefold(channel) == "#me.chat.alice" {
				select {
				case f.joined <- struct{}{}:
				default:
				}
			}
		case strings.HasPrefix(upper, "REDACT #ME.CHAT.ALICE "):
			params := strings.Fields(line)
			if len(params) >= 3 {
				f.redactions <- params[2]
			}
		case strings.HasPrefix(upper, "PRIVMSG #ME.CHAT.ALICE :"):
			f.outbound <- capturedOutbound{text: line[strings.Index(line, " :")+2:], tags: tags}
			if ergoMsgID := tags["+dickord/ergo-msgid"]; ergoMsgID != "" {
				f.sendTo(conn, "@+dickord/discord-msgid="+testOutboundDiscordMessageID+";+reply="+ergoMsgID+" :fake TAGMSG #me.chat.alice")
			}
		case strings.HasPrefix(upper, "TOPIC #ME.CHAT.ALICE"):
			args := ""
			if index := strings.Index(line, " :"); index >= 0 {
				args = line[index+2:]
			}
			f.topics <- args
		case strings.HasPrefix(upper, "QUIT"):
			return
		}
	}
}

func (f *fakeRDirCD) send(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writer != nil {
		_, _ = f.writer.WriteString(line + "\r\n")
		_ = f.writer.Flush()
	}
}

func (f *fakeRDirCD) sendTo(conn net.Conn, line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.current == conn && f.writer != nil {
		_, _ = f.writer.WriteString(line + "\r\n")
		_ = f.writer.Flush()
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func testCertificate(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ergo.test"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"ergo.test"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return cert, private
}

func writeTestFile(t *testing.T, filename string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filename, content, mode); err != nil {
		t.Fatal(err)
	}
}

func runDocker(t *testing.T, args ...string) {
	t.Helper()
	output, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil && !(len(args) > 0 && args[0] == "rm" && strings.Contains(string(output), "No such container")) {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func testErgoName(t *testing.T) string {
	return "dickord-integration-" + strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())
}

func waitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for " + description)
	}
}

func waitText(t *testing.T, values <-chan string, description string) string {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for " + description)
		return ""
	}
}

func waitOutbound(t *testing.T, values <-chan capturedOutbound, description string) capturedOutbound {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for " + description)
		return capturedOutbound{}
	}
}

func silentLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
