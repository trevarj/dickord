package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestRelayAddressTranslation(t *testing.T) {
	tests := map[string]string{
		"Crispy/discord: hi":       "@Crispy hi",
		"Crispy/discord:hi":        "@Crispy hi",
		"hello Crispy/discord: hi": "hello Crispy/discord: hi",
		"Crispy: hi":               "Crispy: hi",
	}
	for input, expected := range tests {
		if actual := translateRelayAddress(input); actual != expected {
			t.Errorf("translateRelayAddress(%q)=%q, want %q", input, actual, expected)
		}
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
