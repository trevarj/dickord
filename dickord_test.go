package main

import (
	"bufio"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircmsg"
)

func TestChannelSelection(t *testing.T) {
	selector := newChannelSelector(ChannelConfig{
		DMIncludePattern: "me.*",
		GuildInclude:     []string{"work.general", "work.dev.*"},
	})
	tests := map[string]bool{
		"#me.chat.alice":        true,
		"#me.chat.group":        true,
		"#work.general":         true,
		"#work.dev.thread":      true,
		"#work.random":          false,
		"#rdircd.control":       true,
		"#rdircd.debug":         false,
		"#rdircd.monitor":       false,
		"#rdircd.monitor.work":  false,
		"#rdircd.leftover.work": false,
		"#rdircd.voice":         false,
	}
	for channel, expected := range tests {
		if actual := selector.selected(channel); actual != expected {
			t.Errorf("selected(%q)=%v, want %v", channel, actual, expected)
		}
	}
}

func TestChannelAliases(t *testing.T) {
	aliases := map[string]string{"me.chat.ABC123": "me.chat.Friends+Family"}
	got := aliasedSource("#ME.CHAT.abc123", aliases)
	if got != "#me.chat.Friends+Family" {
		t.Fatalf("aliased source=%q", got)
	}
	if destination := destinationName(got, "discord.", 64); destination != "#discord.me.chat.Friends+Family" {
		t.Fatalf("aliased destination=%q", destination)
	}
}

func TestDestinationNames(t *testing.T) {
	if got := destinationName("#rdircd.control", "discord.", 64); got != "#discord.control" {
		t.Fatalf("control destination=%q", got)
	}
	if got := destinationName("#me.chat.Alice Smith", "discord.", 64); got != "#discord.me.chat.Alice_20Smith" {
		t.Fatalf("encoded destination=%q", got)
	}
	long := destinationName("#guild."+strings.Repeat("x", 100), "discord.", 40)
	if len(long) != 40 || !strings.Contains(long, "-") {
		t.Fatalf("truncated destination=%q len=%d", long, len(long))
	}
	if again := destinationName("#guild."+strings.Repeat("x", 100), "discord.", 40); again != long {
		t.Fatalf("destination not deterministic: %q != %q", again, long)
	}
	collision := collisionName("#discord.same", "#other", 64)
	if collision == "#discord.same" || collision != collisionName("#discord.same", "#other", 64) {
		t.Fatalf("bad collision name %q", collision)
	}
}

func TestRelayNick(t *testing.T) {
	got := relayNick("123 Alice 🌈", 32)
	if got != "u-123-Alice/discord" {
		t.Fatalf("relay nick=%q", got)
	}
	long := relayNick(strings.Repeat("ø", 60), 24)
	if len(long) > 24 || !strings.HasSuffix(long, "/discord") {
		t.Fatalf("long relay nick=%q len=%d", long, len(long))
	}
}

func TestAuthorizedMessage(t *testing.T) {
	owners := map[string]struct{}{"owner": {}}
	tests := []struct {
		name string
		tags map[string]string
		want bool
	}{
		{"owner", map[string]string{"account": "owner"}, true},
		{"casefolded owner", map[string]string{"account": "OwNeR"}, true},
		{"missing", nil, false},
		{"logged out", map[string]string{"account": "*"}, false},
		{"wrong owner", map[string]string{"account": "mallory"}, false},
		{"relay loop", map[string]string{"account": "owner", "draft/relaymsg": "Dickord"}, false},
		{"legacy relay loop", map[string]string{"account": "owner", "relaymsg": "Dickord"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg := ircmsg.MakeMessage(test.tags, "owner!u@h", "PRIVMSG", "#discord.test", "hello")
			_, _, got := authorizedMessage(msg, owners)
			if got != test.want {
				t.Fatalf("authorized=%v, want %v", got, test.want)
			}
		})
	}
}

func TestRelayResponseErrors(t *testing.T) {
	if !batchHasError(&ircevent.Batch{Message: ircmsg.MakeMessage(nil, "server", "481", "no privileges")}) {
		t.Fatal("numeric error was not detected")
	}
	if !batchHasError(&ircevent.Batch{Items: []*ircevent.Batch{{Message: ircmsg.MakeMessage(nil, "server", "FAIL", "RELAYMSG")}}}) {
		t.Fatal("nested FAIL was not detected")
	}
	if batchHasError(&ircevent.Batch{Message: ircmsg.MakeMessage(nil, "server", "ACK", "RELAYMSG")}) {
		t.Fatal("ACK was treated as an error")
	}
}

func TestPlaybackRejected(t *testing.T) {
	for _, tag := range []string{"batch", "znc.in/playback", "draft/chathistory"} {
		msg := ircmsg.MakeMessage(map[string]string{tag: "1"}, "owner!u@h", "PRIVMSG", "#x", "old")
		if !isPlayback(msg) {
			t.Fatalf("tag %q was not recognized as playback", tag)
		}
	}
}

func TestDiscordDeletionParsing(t *testing.T) {
	if !isDiscordDeletion("--- message was deleted [2026-08-27T00:00:00.000Z]") {
		t.Fatal("deletion annotation was not recognized")
	}
	if isDiscordDeletion("message was edited") {
		t.Fatal("non-deletion was recognized")
	}
}

func TestDiscordReactionParsing(t *testing.T) {
	reaction, ok := parseDiscordReaction("--- reacts: +👍 :: [2026-08-27T00:00:00.000Z] <Alice> hello world")
	if !ok || !reaction.add || reaction.emoji != "👍" || reaction.originalNick != "Alice" || reaction.originalText != "hello world" {
		t.Fatalf("unexpected reaction: %+v ok=%v", reaction, ok)
	}
	reaction, ok = parseDiscordReaction("reacts: +✅ [2026-08-27T00:00:00.000Z]")
	if !ok || !reaction.add || reaction.emoji != "✅" || reaction.originalNick != "" {
		t.Fatalf("unexpected unreferenced reaction: %+v ok=%v", reaction, ok)
	}
	reaction, ok = parseDiscordReaction("reacts: -🔥 :: [2026-08-27T00:00:00.000Z] <Bob> text")
	if !ok || reaction.add || reaction.emoji != "🔥" {
		t.Fatalf("unexpected unreaction: %+v ok=%v", reaction, ok)
	}
	if _, ok := parseDiscordReaction("reacts: -all :: [time] <Bob> text"); ok {
		t.Fatal("remove-all should fall back to text")
	}
}

func TestReactionTags(t *testing.T) {
	msg := ircmsg.MakeMessage(map[string]string{"+draft/react": "👍", "+reply": "abc"}, "owner!u@h", "TAGMSG", "#x")
	emoji, add, ok := reactionTag(msg)
	if !ok || !add || emoji != "👍" {
		t.Fatalf("reactionTag=(%q,%v,%v)", emoji, add, ok)
	}
	msg = ircmsg.MakeMessage(map[string]string{"+draft/unreact": "👍", "+reply": "abc"}, "owner!u@h", "TAGMSG", "#x")
	emoji, add, ok = reactionTag(msg)
	if !ok || add || emoji != "👍" {
		t.Fatalf("unreactionTag=(%q,%v,%v)", emoji, add, ok)
	}
}

func TestRDirCDChannelDescriptorCaching(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rdircd := &ircevent.Connection{}
	message := ircmsg.MakeMessage(
		map[string]string{"+dickord/channel": `{"v":1}`},
		"core!u@rdircd", "TAGMSG", "#source",
	)
	newTestBridge := func() *Bridge {
		bridge := newBridge(RuntimeConfig{Config: Config{Channels: ChannelConfig{CatchUpLimit: 1}}}, logger)
		bridge.rdircd = rdircd
		bridge.ergo = &ircevent.Connection{}
		bridge.ergoRegistered = true
		bridge.operReady = true
		return bridge
	}

	t.Run("unmapped", func(t *testing.T) {
		bridge := newTestBridge()
		bridge.onRDirCDTagMessage(rdircd, message)
		if len(bridge.sourceToDest) != 0 {
			t.Fatalf("descriptor synthesized a mapping: %v", bridge.sourceToDest)
		}
		if len(bridge.descriptors) != 0 {
			t.Fatalf("unmapped descriptor was cached: %v", bridge.descriptors)
		}
	})

	t.Run("old rdircd connection", func(t *testing.T) {
		bridge := newTestBridge()
		bridge.sourceToDest["#source"] = "#destination"
		bridge.onRDirCDTagMessage(&ircevent.Connection{}, message)
		if len(bridge.descriptors) != 0 {
			t.Fatalf("descriptor from old connection was cached: %v", bridge.descriptors)
		}
	})

	t.Run("mapped current connection without metadata capability", func(t *testing.T) {
		bridge := newTestBridge()
		bridge.sourceToDest["#source"] = "#destination"
		message := ircmsg.MakeMessage(map[string]string{
			"+dickord/channel":       `{"v":1}`,
			"+dickord/guild-icon":    "https://example.test/icon.png",
			"+dickord/discord-msgid": "123456789012345678",
			"+reply":                 "ergo-message",
		}, "core!u@rdircd", "TAGMSG", "#source")
		bridge.onRDirCDTagMessage(rdircd, message)
		if got := bridge.descriptors["#source"]; got != `{"v":1}` {
			t.Fatalf("cached descriptor=%q", got)
		}
		if ref := bridge.discordRefs["ergo-message"]; ref.source != "#source" || ref.messageID != "123456789012345678" {
			t.Fatalf("non-metadata TAGMSG handling broke without the capability: %+v", ref)
		}
	})

	t.Run("empty descriptor", func(t *testing.T) {
		bridge := newTestBridge()
		bridge.sourceToDest["#source"] = "#destination"
		bridge.descriptors["#source"] = `{"v":1}`
		bridge.onRDirCDTagMessage(rdircd, ircmsg.MakeMessage(
			map[string]string{"+dickord/channel": ""},
			"core!u@rdircd", "TAGMSG", "#source",
		))
		if _, cached := bridge.descriptors["#source"]; cached {
			t.Fatal("empty descriptor remained cached")
		}
	})
}

func TestChannelSnapshotRequest(t *testing.T) {
	tests := []struct {
		name     string
		tags     map[string]string
		params   []string
		ready    bool
		current  bool
		wantSeed bool
	}{
		{
			name:     "valid",
			tags:     map[string]string{"account": "owner", "+dickord/channel-request": "1", "+draft/react": "👍"},
			params:   []string{"#control"},
			ready:    true,
			current:  true,
			wantSeed: true,
		},
		{
			name:    "unknown version",
			tags:    map[string]string{"account": "owner", "+dickord/channel-request": "2", "+draft/react": "👍"},
			params:  []string{"#control"},
			ready:   true,
			current: true,
		},
		{
			name:    "wrong target",
			tags:    map[string]string{"account": "owner", "+dickord/channel-request": "1"},
			params:  []string{"#destination"},
			ready:   true,
			current: true,
		},
		{
			name:    "no target",
			tags:    map[string]string{"account": "owner", "+dickord/channel-request": "1"},
			ready:   true,
			current: true,
		},
		{
			name:    "multiple targets",
			tags:    map[string]string{"account": "owner", "+dickord/channel-request": "1"},
			params:  []string{"#control", "#destination"},
			ready:   true,
			current: true,
		},
		{
			name:    "historical",
			tags:    map[string]string{"account": "owner", "+dickord/channel-request": "1", "batch": "history"},
			params:  []string{"#control"},
			ready:   true,
			current: true,
		},
		{
			name:    "unauthorized",
			tags:    map[string]string{"account": "mallory", "+dickord/channel-request": "1"},
			params:  []string{"#control"},
			ready:   true,
			current: true,
		},
		{
			name:    "relay loop",
			tags:    map[string]string{"account": "owner", "+dickord/channel-request": "1", "draft/relaymsg": "other"},
			params:  []string{"#control"},
			ready:   true,
			current: true,
		},
		{
			name:    "rdircd not ready",
			tags:    map[string]string{"account": "owner", "+dickord/channel-request": "1"},
			params:  []string{"#control"},
			current: true,
		},
		{
			name:   "old Ergo connection",
			tags:   map[string]string{"account": "owner", "+dickord/channel-request": "1"},
			params: []string{"#control"},
			ready:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bridge, capture := newRelayTestBridge(t)
			bridge.cfg.OwnerAccountsSet = map[string]struct{}{"owner": {}}
			bridge.rdircdReady = test.ready
			bridge.destToSource["#control"] = "#rdircd.control"
			bridge.destToSource["#destination"] = "#source"
			bridge.descriptors["#source"] = relayTestDescriptor
			conn := &ircevent.Connection{}
			if test.current {
				conn = bridge.ergo
			}
			bridge.onErgoTagMessage(conn, ircmsg.MakeMessage(
				test.tags, "owner!u@host", "TAGMSG", test.params...,
			))
			messages := capture()
			if !test.wantSeed {
				if len(messages) != 0 {
					t.Fatalf("invalid snapshot request emitted %+v", messages)
				}
				return
			}
			if len(messages) != 1 || messages[0].Command != "TAGMSG" ||
				len(messages[0].Params) != 1 || messages[0].Params[0] != "#destination" {
				t.Fatalf("snapshot seed=%+v", messages)
			}
			if _, got := messages[0].GetTag("+dickord/channel"); got != relayTestDescriptor {
				t.Fatalf("snapshot descriptor=%q, want %q", got, relayTestDescriptor)
			}
		})
	}
}

func TestNewRDirCDConnectionDropsOldSnapshot(t *testing.T) {
	bridge, capture := newRelayTestBridge(t)
	rdircd := bridge.rdircd
	bridge.sourceToDest["#old"] = "#old-destination"
	bridge.destToSource["#discord.control"] = "#rdircd.control"
	bridge.descriptors["#old"] = `{"v":1}`

	bridge.onRDirCDRegistered(rdircd)
	_ = capture()
	bridge.cfg.OwnerAccountsSet = map[string]struct{}{"owner": {}}
	bridge.rdircdReady = true
	bridge.onErgoTagMessage(bridge.ergo, ircmsg.MakeMessage(
		map[string]string{"account": "owner", "+dickord/channel-request": "1"},
		"owner!u@host", "TAGMSG", "#discord.control",
	))
	if messages := capture(); len(messages) != 0 {
		t.Fatalf("new rdircd connection exposed stale snapshot: %+v", messages)
	}
}

const relayTestDescriptor = `{"v":1,"channel_id":"123456789012345678","guild_id":"223456789012345678","guild_name":"Example; Server 🌈","type":0,"is_thread":false,"channel_name":"release.notes_20 \\ 🌈","guild_icon_url":"https://example.test/icon.png"}`

func TestRDirCDChannelDescriptorTransport(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run("metadata="+strconv.FormatBool(metadata), func(t *testing.T) {
			var caps []string
			if metadata {
				caps = append(caps, "draft/metadata-2")
			}
			bridge, capture := newRelayTestBridge(t, caps...)
			for _, icon := range []string{"https://example.test/icon.png", ""} {
				descriptor := relayTestDescriptor
				if icon == "" {
					descriptor = strings.Replace(descriptor, `"https://example.test/icon.png"`, "null", 1)
				}
				bridge.onRDirCDTagMessage(bridge.rdircd, ircmsg.MakeMessage(map[string]string{
					"+dickord/channel":    descriptor,
					"+dickord/guild-icon": icon,
				}, "core!u@rdircd", "TAGMSG", "#source"))
				messages := capture()
				wantCount := 1
				if metadata {
					wantCount++
				}
				if len(messages) != wantCount {
					t.Fatalf("descriptor/icon update emitted %d messages, want %d: %+v", len(messages), wantCount, messages)
				}
				seed := messages[0]
				if seed.Command != "TAGMSG" || len(seed.Params) != 1 || seed.Params[0] != "#destination" {
					t.Fatalf("descriptor seed=%+v", seed)
				}
				if _, got := seed.GetTag("+dickord/channel"); got != descriptor {
					t.Fatalf("descriptor seed=%q, want %q", got, descriptor)
				}
				if metadata {
					avatar := messages[1]
					want := "#destination SET avatar"
					if icon != "" {
						want += " " + icon
					}
					if avatar.Command != "METADATA" || strings.Join(avatar.Params, " ") != want {
						t.Fatalf("avatar metadata=%+v, want %q", avatar, want)
					}
				}
			}
		})
	}
}

func TestRDirCDChannelDescriptorSizeBounds(t *testing.T) {
	rawLimit := relayTestDescriptor + strings.Repeat("\t", 2048-len(relayTestDescriptor))
	padding := 3072 - len(ircmsg.EscapeTagValue(relayTestDescriptor))
	escapedLimit := relayTestDescriptor + strings.Repeat(" ", padding/2) + strings.Repeat("\t", padding%2)
	for _, test := range []struct {
		name     string
		value    string
		accepted bool
	}{
		{"JSON byte limit", rawLimit, true},
		{"JSON byte overflow", rawLimit + "\t", false},
		{"escaped byte limit", escapedLimit, true},
		{"escaped byte overflow", escapedLimit + "\t", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			bridge, capture := newRelayTestBridge(t)
			bridge.descriptors["#source"] = relayTestDescriptor
			bridge.onRDirCDTagMessage(bridge.rdircd, ircmsg.MakeMessage(
				map[string]string{"+dickord/channel": test.value},
				"core!u@rdircd", "TAGMSG", "#source",
			))
			bridge.relayToErgo("#destination", "Alice", "unchanged body", false, discordMessageRef{source: "#source"}, "", false)
			messages := capture()
			wantDescriptor := relayTestDescriptor
			wantCommands := []string{"RELAYMSG"}
			if test.accepted {
				wantDescriptor = test.value
				wantCommands = []string{"TAGMSG", "RELAYMSG"}
			}
			if len(messages) != len(wantCommands) {
				t.Fatalf("descriptor update emitted %+v, want %v", messages, wantCommands)
			}
			for i, message := range messages {
				if message.Command != wantCommands[i] || message.Params[0] != "#destination" {
					t.Fatalf("descriptor update message=%+v, want %s #destination", message, wantCommands[i])
				}
				if _, got := message.GetTag("+dickord/channel"); got != wantDescriptor {
					t.Fatalf("descriptor was truncated or invalid update replaced cached state: got %q, want %q", got, wantDescriptor)
				}
			}
			relay := messages[len(messages)-1]
			if len(relay.Params) != 3 || relay.Params[1] != "Alice/discord" || relay.Params[2] != "unchanged body" {
				t.Fatalf("descriptor size changed relay attribution/body: %+v", relay)
			}
		})
	}
}

func TestRelayMessageTagBudget(t *testing.T) {
	padding := 3072 - len(ircmsg.EscapeTagValue(relayTestDescriptor))
	descriptor := relayTestDescriptor + strings.Repeat(" ", padding/2) + strings.Repeat("\t", padding%2)
	for _, labeled := range []bool{false, true} {
		for _, extra := range []int{0, 1} {
			t.Run("labeled="+strconv.FormatBool(labeled)+"/overflow="+strconv.Itoa(extra), func(t *testing.T) {
				var caps []string
				if labeled {
					caps = append(caps, "batch", "labeled-response")
				}
				bridge, capture := newRelayTestBridge(t, caps...)
				bridge.onRDirCDTagMessage(bridge.rdircd, ircmsg.MakeMessage(
					map[string]string{"+dickord/channel": descriptor},
					"core!u@rdircd", "TAGMSG", "#source",
				))
				const reply = "original-message"
				tags := map[string]string{
					"+dickord/nonce":   "1",
					"+dickord/channel": descriptor,
					"+dickord/avatar":  "https://example.test/",
					"+reply":           reply,
				}
				if labeled {
					// The first label on a fresh connection occupies one byte.
					tags["label"] = "1"
				}
				probe := ircmsg.MakeMessage(tags, "", "RELAYMSG", "#destination", "Alice/discord", "hello 🌈")
				line, err := probe.LineBytesStrict(true, 512)
				if err != nil {
					t.Fatal(err)
				}
				tagBytes := strings.IndexByte(string(line), ' ') - 1
				avatar := tags["+dickord/avatar"] + strings.Repeat("x", ircmsg.MaxlenClientTagData-tagBytes+extra)
				bridge.ergoByDiscord[discordRefKey{source: "#source", messageID: "323456789012345678"}] = []string{reply}
				bridge.relayToErgo("#destination", "Alice", "hello 🌈", false, discordMessageRef{
					source: "#source", replyMessageID: "323456789012345678",
				}, avatar, true)
				messages := capture()
				if len(messages) != 2 || messages[0].Command != "TAGMSG" {
					t.Fatalf("expected a complete seed and exactly one relay, got %+v", messages)
				}
				if _, got := messages[0].GetTag("+dickord/channel"); got != descriptor {
					t.Fatalf("standalone seed lost complete descriptor: %q", got)
				}
				message := messages[1]
				if message.Command != "RELAYMSG" || len(message.Params) != 3 || message.Params[0] != "#destination" ||
					message.Params[1] != "Alice/discord" || message.Params[2] != "hello 🌈" {
					t.Fatalf("optional descriptor changed relay attribution/body: %+v", message)
				}
				for key, want := range map[string]string{"+dickord/nonce": "1", "+reply": reply, "+dickord/avatar": avatar} {
					if present, got := message.GetTag(key); !present || got != want {
						t.Fatalf("relay lost required tag %q: got %q, want %q", key, got, want)
					}
				}
				if present, label := message.GetTag("label"); present != labeled || (labeled && label == "") {
					t.Fatalf("relay lost actual label: present=%v label=%q", present, label)
				}
				if present, got := message.GetTag("+dickord/channel"); present != (extra == 0) || (present && got != descriptor) {
					t.Fatalf("descriptor budget decision: present=%v descriptor=%q", present, got)
				}
			})
		}
	}
}

func newRelayTestBridge(t *testing.T, caps ...string) (*Bridge, func() []ircmsg.Message) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	caps = append([]string{"message-tags"}, caps...)
	conn := &ircevent.Connection{
		Server: listener.Addr().String(), Nick: "dickord", User: "dickord",
		RequestCaps: caps, Timeout: 5 * time.Second, KeepAlive: time.Hour,
		Log: log.New(io.Discard, "", 0),
	}
	serverReady := make(chan net.Conn, 1)
	var reader *bufio.Reader
	handshake := make(chan error, 1)
	go func() {
		server, err := listener.Accept()
		if err != nil {
			handshake <- err
			return
		}
		serverReady <- server
		reader = bufio.NewReader(server)
		handshake <- func() error {
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return err
				}
				message, err := ircmsg.ParseLine(line)
				if err != nil {
					return err
				}
				if message.Command != "CAP" || len(message.Params) == 0 {
					continue
				}
				var response string
				switch message.Params[0] {
				case "LS":
					response = ":server CAP * LS :" + strings.Join(caps, " ")
				case "REQ":
					response = ":server CAP * ACK :" + message.Params[1]
				case "END":
					response = ":server 001 dickord :Welcome\r\n:server 376 dickord :End of MOTD"
				}
				if _, err := io.WriteString(server, response+"\r\n"); err != nil {
					return err
				}
				if message.Params[0] == "END" {
					return nil
				}
			}
		}()
	}()
	if err := conn.Connect(); err != nil {
		t.Fatal(err)
	}
	if err := <-handshake; err != nil {
		t.Fatal(err)
	}
	server := <-serverReady
	stopped := make(chan struct{})
	go func() {
		conn.Loop()
		close(stopped)
	}()
	t.Cleanup(func() {
		conn.Quit()
		_ = server.Close()
		<-stopped
	})
	bridge := newBridge(RuntimeConfig{Config: Config{Channels: ChannelConfig{CatchUpLimit: 1}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bridge.rdircd = &ircevent.Connection{}
	bridge.ergo = conn
	bridge.ergoRegistered, bridge.operReady, bridge.relayReady = true, true, true
	bridge.sourceToDest["#source"] = "#destination"
	return bridge, func() []ircmsg.Message {
		t.Helper()
		if err := conn.Send("PING", "capture-end"); err != nil {
			t.Fatal(err)
		}
		if err := server.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var messages []ircmsg.Message
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			message, err := ircmsg.ParseLineStrict(line, true, 512)
			if err != nil {
				t.Fatalf("invalid client wire line: %v", err)
			}
			if message.Command == "PING" && len(message.Params) == 1 && message.Params[0] == "capture-end" {
				return messages
			}
			messages = append(messages, message)
		}
	}
}

func TestRelayAddressTranslation(t *testing.T) {
	bridge := &Bridge{discordUsers: map[discordUserKey]string{
		{source: "#one", nick: "crispy"}: "123456789012345678",
	}}
	tests := []struct {
		source   string
		input    string
		expected string
	}{
		{"#one", "Crispy/discord: hi", "<@123456789012345678> hi"},
		{"#two", "Crispy/discord: hi", "@Crispy hi"},
		{"#one", "Crispy/discord:hi", "<@123456789012345678> hi"},
		{"#one", "hello Crispy/discord: hi", "hello Crispy/discord: hi"},
		{"#one", "Crispy: hi", "Crispy: hi"},
	}
	for _, test := range tests {
		if actual := bridge.translateRelayAddress(test.source, test.input); actual != test.expected {
			t.Errorf("translateRelayAddress(%q)=%q, want %q", test.input, actual, test.expected)
		}
	}
	if actual := bridge.translateDiscordMentions("#one", "notice how @Crispy looked"); actual != "notice how <@123456789012345678> looked" {
		t.Fatalf("inline mention=%q", actual)
	}
	if actual := bridge.translateDiscordMentions("#two", "notice how @Crispy looked"); actual != "notice how @Crispy looked" {
		t.Fatalf("unknown inline mention=%q", actual)
	}
}

func TestDiscordCorrelationGroupsAndEviction(t *testing.T) {
	bridge := &Bridge{
		cfg:           RuntimeConfig{Config: Config{Channels: ChannelConfig{CatchUpLimit: 300}}},
		sourceToDest:  make(map[string]string),
		discordRefs:   make(map[string]discordMessageRef),
		ergoByDiscord: make(map[discordRefKey][]string),
	}
	for i := 0; i < 17; i++ {
		bridge.sourceToDest["#source."+strconv.Itoa(i)] = "#dest"
	}
	if limit := bridge.discordRefLimitLocked(); limit != 5100 {
		t.Fatalf("correlation limit=%d, want 5100", limit)
	}
	ref := discordMessageRef{source: "#source.0", messageID: "100"}
	bridge.cacheDiscordRefLocked("ergo-first", ref)
	bridge.cacheDiscordRefLocked("ergo-second", ref)
	key := discordRefKey{source: "#source.0", messageID: "100"}
	if got := bridge.ergoByDiscord[key]; len(got) != 2 || got[0] != "ergo-first" || got[1] != "ergo-second" {
		t.Fatalf("correlation group=%v", got)
	}

	bridge.cfg.Channels.CatchUpLimit = 1
	bridge.sourceToDest = map[string]string{"#only": "#dest"}
	for i := 0; i < 4096; i++ {
		id := strconv.Itoa(1000 + i)
		bridge.cacheDiscordRefLocked("ergo-"+id, discordMessageRef{source: "#only", messageID: id})
	}
	if _, exists := bridge.ergoByDiscord[key]; exists {
		t.Fatal("oldest correlation group was not evicted")
	}
	if _, exists := bridge.discordRefs["ergo-first"]; exists {
		t.Fatal("eviction did not remove reverse correlation")
	}
}

func TestTopicCommand(t *testing.T) {
	tests := []struct {
		input string
		args  string
		ok    bool
	}{
		{"!topic", "", true},
		{"!topic log 2h", "log 2h", true},
		{"!topic   info alice  ", "info alice", true},
		{"!topics", "", false},
		{"hello", "", false},
	}
	for _, test := range tests {
		args, ok := topicCommand(test.input)
		if args != test.args || ok != test.ok {
			t.Errorf("topicCommand(%q)=(%q,%v), want (%q,%v)", test.input, args, ok, test.args, test.ok)
		}
	}
}

func TestSplitUTF8(t *testing.T) {
	input := strings.Repeat("hello 🌈 world ", 30)
	parts := splitUTF8(input, 37)
	if len(parts) < 2 {
		t.Fatal("message was not split")
	}
	for _, part := range parts {
		if !utf8.ValidString(part) || len(part) > 37 {
			t.Fatalf("invalid part %q (%d bytes)", part, len(part))
		}
	}
	joined := strings.Join(parts, " ")
	if strings.Join(strings.Fields(joined), " ") != strings.Join(strings.Fields(input), " ") {
		t.Fatalf("split changed content\n got: %q\nwant: %q", joined, input)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) string {
		filename := filepath.Join(dir, name)
		if err := os.WriteFile(filename, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
		return filename
	}
	sasl := write("sasl", "sasl-secret\n")
	oper := write("oper", "oper-secret\n")
	config := `{
		"ergo": {
			"address": "host.docker.internal:6698",
			"tls_server_name": "irc.example.test",
			"nick": "Dickord",
			"account": "Dickord",
			"password_file": ` + quoteJSON(sasl) + `,
			"oper_name": "Dickord",
			"oper_password_file": ` + quoteJSON(oper) + `,
			"owner_accounts": ["owner"]
		},
		"rdircd": {"address": "rdircd:6667"},
		"channels": {
			"guild_include_globs": ["work.general"],
			"catch_up": true
		},
		"reconnect": {"minimum": "1s", "maximum": "30s"}
	}`
	filename := write("config.json", config)
	cfg, err := loadConfig(filename)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ErgoPassword != "sasl-secret" || cfg.OperPassword != "oper-secret" {
		t.Fatal("secret files were not loaded and trimmed")
	}
	if cfg.Channels.DMIncludePattern != "me.*" || cfg.Channels.CatchUpLimit != 300 {
		t.Fatalf("defaults not applied: %+v", cfg.Channels)
	}
}

func quoteJSON(value string) string {
	return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"`
}
