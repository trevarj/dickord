package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ergochat/irc-go/ircevent"
	"github.com/ergochat/irc-go/ircfmt"
	"github.com/ergochat/irc-go/ircmsg"
)

type discordMessageRef struct {
	source         string
	messageID      string
	replyMessageID string
}

type discordRefKey struct {
	source    string
	messageID string
}

type discordUserKey struct {
	source string
	nick   string
}

type pendingRelay struct {
	destination string
	nick        string
	text        string
	discord     discordMessageRef
}

type messageRefKey struct {
	destination string
	nick        string
	text        string
}

type discordReaction struct {
	add          bool
	emoji        string
	originalNick string
	originalText string
}

type pendingDiscordReaction struct {
	source    string
	messageID string
	emoji     string
	add       bool
}

type Bridge struct {
	cfg      RuntimeConfig
	selector channelSelector
	log      *slog.Logger
	uploads  *attachmentUploader

	mu               sync.RWMutex
	ergo             *ircevent.Connection
	ergoRegistered   bool
	operReady        bool
	relayReady       bool
	rdircd           *ircevent.Connection
	rdircdReady      bool
	rdircdRefreshing bool
	rdircdCancel     context.CancelFunc
	rdircdGeneration uint64
	sourceToDest     map[string]string
	destToSource     map[string]string
	descriptors      map[string]string
	watchedSources   map[string]bool
	autoJoinPending  map[string]bool
	ownerNicks       map[string]string
	nextRelayNonce   uint64
	pendingRelays    map[string]pendingRelay
	messageRefs      map[messageRefKey]string
	messageRefOrder  []messageRefKey
	discordRefs      map[string]discordMessageRef
	ergoByDiscord    map[discordRefKey][]string
	discordRefOrder  []discordRefKey
	discordUsers     map[discordUserKey]string
	pendingReactions map[pendingDiscordReaction]time.Time
	pendingDeletions map[discordRefKey]time.Time
}

func newBridge(cfg RuntimeConfig, logger *slog.Logger) *Bridge {
	return &Bridge{
		cfg:              cfg,
		selector:         newChannelSelector(cfg.Channels),
		log:              logger,
		uploads:          newAttachmentUploader(cfg),
		sourceToDest:     make(map[string]string),
		destToSource:     make(map[string]string),
		descriptors:      make(map[string]string),
		watchedSources:   make(map[string]bool),
		autoJoinPending:  make(map[string]bool),
		ownerNicks:       make(map[string]string),
		pendingRelays:    make(map[string]pendingRelay),
		messageRefs:      make(map[messageRefKey]string),
		discordRefs:      make(map[string]discordMessageRef),
		ergoByDiscord:    make(map[discordRefKey][]string),
		discordUsers:     make(map[discordUserKey]string),
		pendingReactions: make(map[pendingDiscordReaction]time.Time),
		pendingDeletions: make(map[discordRefKey]time.Time),
	}
}

func (b *Bridge) Run(ctx context.Context) error {
	delay := b.cfg.Reconnect.Minimum
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		conn := b.newErgoConnection()
		b.mu.Lock()
		b.ergo = conn
		b.mu.Unlock()

		b.log.Info("connecting to Ergo", "address", b.cfg.Ergo.Address)
		if err := conn.Connect(); err != nil {
			b.log.Error("Ergo connection failed", "error", err)
			b.clearErgo(conn)
			if !waitContext(ctx, delay) {
				return nil
			}
			delay = nextDelay(delay, b.cfg.Reconnect.Maximum)
			continue
		}

		delay = b.cfg.Reconnect.Minimum
		done := make(chan struct{})
		go func() {
			conn.Loop()
			close(done)
		}()
		select {
		case <-ctx.Done():
			conn.Quit()
			<-done
			b.stopRDirCD()
			return nil
		case <-done:
			b.clearErgo(conn)
			if !waitContext(ctx, delay) {
				return nil
			}
			delay = nextDelay(delay, b.cfg.Reconnect.Maximum)
		}
	}
}

func (b *Bridge) newErgoConnection() *ircevent.Connection {
	conn := &ircevent.Connection{
		Server:          b.cfg.Ergo.Address,
		Nick:            b.cfg.Ergo.Nick,
		User:            b.cfg.Ergo.Account,
		RealName:        "Discord IRC bridge",
		UseTLS:          true,
		TLSConfig:       &tls.Config{MinVersion: tls.VersionTLS12, ServerName: b.cfg.Ergo.TLSServerName, RootCAs: b.cfg.ErgoRootCAs},
		SASLLogin:       b.cfg.Ergo.Account,
		SASLPassword:    b.cfg.ErgoPassword,
		RequestCaps:     []string{"account-notify", "account-tag", "echo-message", "extended-join", "server-time", "message-tags", "batch", "labeled-response", "draft/message-redaction", "draft/metadata-2", "draft/relaymsg"},
		Timeout:         20 * time.Second,
		KeepAlive:       90 * time.Second,
		ReconnectFreq:   b.cfg.Reconnect.Minimum,
		MaxLineLen:      1024,
		AllowTruncation: false,
		Debug:           false,
		Log:             log.New(slogWriter{b.log, "ergo"}, "", 0),
	}
	conn.AddRawCallback(func(_ string, msg ircmsg.Message, err error) {
		if err == nil {
			b.captureRelayEcho(conn, msg)
		}
	})
	conn.AddConnectCallback(func(ircmsg.Message) { b.onErgoRegistered(conn) })
	conn.AddDisconnectCallback(func(ircmsg.Message) { b.onErgoDisconnected(conn) })
	conn.AddCallback("381", func(ircmsg.Message) { b.onErgoOper(conn) })
	for _, numeric := range []string{"464", "491"} {
		conn.AddCallback(numeric, func(msg ircmsg.Message) { b.onErgoOperFailure(conn, msg.Command) })
	}
	conn.AddCallback("PRIVMSG", func(msg ircmsg.Message) { b.onErgoMessage(conn, msg) })
	conn.AddCallback("TAGMSG", func(msg ircmsg.Message) { b.onErgoTagMessage(conn, msg) })
	conn.AddCallback("REDACT", func(msg ircmsg.Message) { b.onErgoRedact(conn, msg) })
	conn.AddCallback("JOIN", func(msg ircmsg.Message) { b.onErgoJoin(conn, msg) })
	return conn
}

func (b *Bridge) onErgoRegistered(conn *ircevent.Connection) {
	caps := conn.AcknowledgedCaps()
	if _, ok := caps["account-tag"]; !ok {
		b.log.Error("Ergo did not negotiate required account-tag capability; bridge remains closed")
		return
	}

	b.mu.Lock()
	if b.ergo != conn {
		b.mu.Unlock()
		return
	}
	b.ergoRegistered = true
	b.operReady = false
	b.relayReady = false
	destinations := make([]string, 0, len(b.destToSource))
	for destination := range b.destToSource {
		destinations = append(destinations, destination)
	}
	b.mu.Unlock()

	for _, destination := range destinations {
		_ = conn.Join(destination)
	}
	if err := conn.Send("OPER", b.cfg.Ergo.OperName, b.cfg.OperPassword); err != nil {
		b.log.Error("failed to submit Ergo OPER authentication", "error", err)
	}
}

func (b *Bridge) onErgoOper(conn *ircevent.Connection) {
	b.mu.Lock()
	if b.ergo != conn || !b.ergoRegistered {
		b.mu.Unlock()
		return
	}
	b.operReady = true
	_, b.relayReady = conn.AcknowledgedCaps()["draft/relaymsg"]
	relay := b.relayReady
	b.mu.Unlock()
	b.log.Info("Ergo authorization ready", "relaymsg", relay)
	b.startRDirCD()
}

func (b *Bridge) onErgoOperFailure(conn *ircevent.Connection, numeric string) {
	b.mu.Lock()
	if b.ergo != conn {
		b.mu.Unlock()
		return
	}
	b.operReady = false
	b.relayReady = false
	b.mu.Unlock()
	b.stopRDirCD()
	b.log.Error("Ergo OPER authentication failed; bridge remains closed", "numeric", numeric)
}

func (b *Bridge) onErgoDisconnected(conn *ircevent.Connection) {
	b.mu.Lock()
	if b.ergo != conn {
		b.mu.Unlock()
		return
	}
	b.ergoRegistered = false
	b.operReady = false
	b.relayReady = false
	b.ownerNicks = make(map[string]string)
	b.mu.Unlock()
	b.stopRDirCD()
	b.log.Warn("Ergo disconnected; rdircd leg closed")
}

func (b *Bridge) clearErgo(conn *ircevent.Connection) {
	b.mu.Lock()
	if b.ergo == conn {
		b.ergo = nil
		b.ergoRegistered = false
		b.operReady = false
		b.relayReady = false
		b.ownerNicks = make(map[string]string)
	}
	b.mu.Unlock()
	b.stopRDirCD()
}

func (b *Bridge) startRDirCD() {
	b.mu.Lock()
	if b.rdircdCancel != nil || !b.ergoRegistered || !b.operReady {
		b.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	b.rdircdCancel = cancel
	b.rdircdGeneration++
	generation := b.rdircdGeneration
	b.mu.Unlock()
	go b.runRDirCD(ctx, generation)
}

func (b *Bridge) stopRDirCD() {
	b.mu.Lock()
	cancel := b.rdircdCancel
	b.rdircdCancel = nil
	b.rdircdReady = false
	b.rdircdRefreshing = false
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (b *Bridge) runRDirCD(ctx context.Context, generation uint64) {
	defer func() {
		b.mu.Lock()
		if b.rdircdGeneration == generation {
			b.rdircdCancel = nil
		}
		b.mu.Unlock()
	}()

	delay := b.cfg.Reconnect.Minimum
	for ctx.Err() == nil {
		connCtx, cancel := context.WithCancel(ctx)
		conn := b.newRDirCDConnection(connCtx)
		b.mu.Lock()
		b.rdircd = conn
		b.rdircdReady = false
		b.mu.Unlock()
		b.log.Info("connecting to rdircd", "address", b.cfg.RDirCD.Address)
		if err := conn.Connect(); err != nil {
			cancel()
			b.log.Error("rdircd connection failed", "error", err)
			b.clearRDirCD(conn)
			if !waitContext(ctx, delay) {
				return
			}
			delay = nextDelay(delay, b.cfg.Reconnect.Maximum)
			continue
		}

		done := make(chan struct{})
		go func() {
			conn.Loop()
			close(done)
		}()
		select {
		case <-ctx.Done():
			conn.Quit()
			<-done
		case <-done:
		}
		cancel()
		b.clearRDirCD(conn)
		return
	}
}

func (b *Bridge) newRDirCDConnection(ctx context.Context) *ircevent.Connection {
	conn := &ircevent.Connection{
		Server:          b.cfg.RDirCD.Address,
		Nick:            b.cfg.RDirCD.Nick,
		User:            b.cfg.RDirCD.Nick,
		RealName:        "Dickord relay",
		RequestCaps:     []string{"message-tags"},
		Timeout:         20 * time.Second,
		KeepAlive:       90 * time.Second,
		ReconnectFreq:   b.cfg.Reconnect.Minimum,
		MaxLineLen:      1024,
		AllowTruncation: false,
		Debug:           false,
		Log:             log.New(slogWriter{b.log, "rdircd"}, "", 0),
	}
	conn.AddConnectCallback(func(ircmsg.Message) { b.onRDirCDRegistered(conn) })
	conn.AddDisconnectCallback(func(ircmsg.Message) { b.onRDirCDDisconnected(conn) })
	conn.AddCallback("322", func(msg ircmsg.Message) { b.onRDirCDListEntry(conn, msg) })
	conn.AddCallback("323", func(ircmsg.Message) { b.onRDirCDListEnd(conn) })
	conn.AddCallback("JOIN", func(msg ircmsg.Message) { b.onRDirCDJoin(conn, msg) })
	// ponytail: one bounded worker preserves attachment/text order; parallelize only
	// if upload throughput requires it. Backpressure bounds pending message memory.
	messages := make(chan ircmsg.Message, 64)
	enqueue := func(msg ircmsg.Message) {
		select {
		case messages <- msg:
		case <-ctx.Done():
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-messages:
				b.onRDirCDMessage(ctx, conn, msg, msg.Command == "NOTICE")
			}
		}
	}()
	conn.AddCallback("PRIVMSG", enqueue)
	conn.AddCallback("NOTICE", enqueue)
	conn.AddCallback("TAGMSG", func(msg ircmsg.Message) { b.onRDirCDTagMessage(conn, msg) })
	conn.AddCallback("TOPIC", func(msg ircmsg.Message) { b.onRDirCDTopic(conn, msg) })
	return conn
}

func (b *Bridge) onRDirCDRegistered(conn *ircevent.Connection) {
	b.mu.Lock()
	valid := b.rdircd == conn && b.ergoRegistered && b.operReady
	if valid {
		b.rdircdReady = false
		b.rdircdRefreshing = false
		b.descriptors = make(map[string]string)
		b.watchedSources = make(map[string]bool)
	}
	b.mu.Unlock()
	if !valid {
		conn.Quit()
		return
	}
	b.ensureMapping(conn, "#rdircd.control", true)
	if err := conn.Send("LIST"); err != nil {
		b.log.Error("failed to start rdircd channel discovery", "error", err)
	}
}

func (b *Bridge) onRDirCDDisconnected(conn *ircevent.Connection) {
	b.mu.Lock()
	if b.rdircd == conn {
		b.rdircdReady = false
		b.rdircdRefreshing = false
	}
	b.mu.Unlock()
	b.log.Warn("rdircd disconnected")
}

func (b *Bridge) clearRDirCD(conn *ircevent.Connection) {
	b.mu.Lock()
	if b.rdircd == conn {
		b.rdircd = nil
		b.rdircdReady = false
		b.rdircdRefreshing = false
	}
	b.mu.Unlock()
}

func (b *Bridge) onRDirCDListEntry(conn *ircevent.Connection, msg ircmsg.Message) {
	for _, param := range msg.Params {
		if !strings.HasPrefix(param, "#") {
			continue
		}
		if destination, selected := b.ensureMapping(conn, param, true); selected && len(msg.Params) > 0 {
			topic := msg.Params[len(msg.Params)-1]
			b.setErgoTopic(destination, topic)
			if strings.Contains(topic, "<W>") {
				b.mu.Lock()
				b.watchedSources[ircCasefold(param)] = true
				b.mu.Unlock()
			}
		}
		return
	}
}

func (b *Bridge) onRDirCDListEnd(conn *ircevent.Connection) {
	b.mu.Lock()
	if b.rdircd != conn {
		b.mu.Unlock()
		return
	}
	b.rdircdRefreshing = false
	if !b.ergoRegistered || !b.operReady {
		b.mu.Unlock()
		return
	}
	initial := !b.rdircdReady
	b.rdircdReady = true
	sources := make([]string, 0, len(b.sourceToDest))
	for source := range b.sourceToDest {
		if ircCasefold(strings.TrimPrefix(source, "#")) != "rdircd.control" {
			sources = append(sources, source)
		}
	}
	b.mu.Unlock()

	if b.cfg.Channels.CatchUp {
		if initial {
			if err := conn.Privmsg("#rdircd.control", fmt.Sprintf(
				"set -s discord-msg-history-fetch-limit %d", b.cfg.Channels.CatchUpLimit)); err != nil {
				b.log.Error("failed to configure rdircd history limit", "error", err)
			}
		}
		for _, source := range sources {
			b.enableWatch(conn, source)
		}
	}
	b.log.Info("rdircd channel discovery complete", "mapped", len(sources), "catch_up", b.cfg.Channels.CatchUp)
}

func (b *Bridge) onRDirCDJoin(conn *ircevent.Connection, msg ircmsg.Message) {
	if msg.Nick() != conn.CurrentNick() || len(msg.Params) == 0 {
		return
	}
	source := msg.Params[0]
	if !b.selector.selected(source) {
		_ = conn.Part(source)
		return
	}
	if _, selected := b.ensureMapping(conn, source, false); selected && b.cfg.Channels.CatchUp {
		b.enableWatch(conn, source)
	}
}

func (b *Bridge) enableWatch(conn *ircevent.Connection, source string) {
	key := ircCasefold(source)
	b.mu.Lock()
	if b.watchedSources[key] {
		b.mu.Unlock()
		return
	}
	b.watchedSources[key] = true
	b.mu.Unlock()

	// Upstream's control-channel watch-by-name path is broken for private
	// channels. The channel-local TOPIC command uses the correct channel ID.
	if err := conn.Send("TOPIC", source, "log watch-silent"); err != nil {
		b.mu.Lock()
		delete(b.watchedSources, key)
		b.mu.Unlock()
		b.log.Error("failed to enable rdircd watch", "channel", source, "error", err)
	}
}

func (b *Bridge) ensureMapping(conn *ircevent.Connection, source string, joinSource bool) (string, bool) {
	if !strings.HasPrefix(source, "#") {
		source = "#" + source
	}
	if !b.selector.selected(source) {
		return "", false
	}
	sourceKey := ircCasefold(source)

	b.mu.Lock()
	if destination := b.sourceToDest[sourceKey]; destination != "" {
		ergo := b.ergo
		b.mu.Unlock()
		if ergo != nil {
			_ = ergo.Join(destination)
			b.autoJoinOwners(ergo, destination)
		}
		if joinSource {
			_ = conn.Join(source)
		}
		return destination, true
	}
	ergo := b.ergo
	if ergo == nil {
		b.mu.Unlock()
		return "", false
	}
	maxLen := channelLength(ergo.ISupport(), b.cfg.Channels.MaxNameLength)
	destination := destinationName(aliasedSource(source, b.cfg.Channels.Aliases), b.cfg.Channels.Prefix, maxLen)
	destKey := ircCasefold(destination)
	if existing := b.destToSource[destKey]; existing != "" && existing != sourceKey {
		destination = collisionName(destination, source, maxLen)
		destKey = ircCasefold(destination)
	}
	b.sourceToDest[sourceKey] = destination
	b.destToSource[destKey] = source
	b.mu.Unlock()

	_ = ergo.Join(destination)
	b.autoJoinOwners(ergo, destination)
	if joinSource {
		_ = conn.Join(source)
	}
	b.log.Debug("mapped channel", "source", source, "destination", destination)
	return destination, true
}

func (b *Bridge) onRDirCDTagMessage(conn *ircevent.Connection, msg ircmsg.Message) {
	if len(msg.Params) < 1 {
		return
	}
	source := msg.Params[0]
	if present, channel := msg.GetTag("+dickord/channel"); present {
		b.setErgoChannelDescriptor(conn, source, channel)
	}
	if present, avatar := msg.GetTag("+dickord/guild-icon"); present {
		b.setErgoChannelMetadata(conn, source, "avatar", avatar)
	}
	_, discordMsgID := msg.GetTag("+dickord/discord-msgid")
	_, ergoMsgID := msg.GetTag("+reply")
	if discordMsgID == "" || ergoMsgID == "" {
		return
	}
	b.mu.Lock()
	if b.rdircd == conn {
		b.cacheDiscordRefLocked(ergoMsgID, discordMessageRef{source: source, messageID: discordMsgID})
	}
	b.mu.Unlock()
}

func (b *Bridge) setErgoChannelDescriptor(rdircd *ircevent.Connection, source, value string) {
	if len(value) > 2048 || len(ircmsg.EscapeTagValue(value)) > 3072 {
		return
	}
	sourceKey := ircCasefold(source)
	b.mu.Lock()
	destination := b.sourceToDest[sourceKey]
	if b.rdircd != rdircd || destination == "" {
		b.mu.Unlock()
		return
	}
	if value == "" {
		delete(b.descriptors, sourceKey)
		b.mu.Unlock()
		return
	}
	b.descriptors[sourceKey] = value
	ergo := b.ergo
	ready := b.ergoRegistered && b.operReady
	b.mu.Unlock()
	if ergo == nil || !ready {
		return
	}
	if err := ergo.SendWithTags(map[string]string{"+dickord/channel": value}, "TAGMSG", destination); err != nil {
		b.log.Warn("failed to mirror channel descriptor tag", "channel", destination, "error", err)
	}
}

func (b *Bridge) setErgoChannelMetadata(rdircd *ircevent.Connection, source, key, value string) {
	b.mu.RLock()
	ergo := b.ergo
	destination := b.sourceToDest[ircCasefold(source)]
	current := b.rdircd == rdircd
	ready := b.ergoRegistered && b.operReady
	b.mu.RUnlock()
	if !current || ergo == nil || destination == "" || !ready {
		return
	}
	if _, acknowledged := ergo.AcknowledgedCaps()["draft/metadata-2"]; !acknowledged {
		return
	}
	params := []string{destination, "SET", key}
	if value != "" {
		params = append(params, value)
	}
	if err := ergo.Send("METADATA", params...); err != nil {
		b.log.Warn("failed to mirror channel metadata", "channel", destination, "key", key, "error", err)
	}
}

func (b *Bridge) discordRefLimitLocked() int {
	mapped := len(b.sourceToDest)
	if _, control := b.sourceToDest[ircCasefold("#rdircd.control")]; control {
		mapped--
	}
	return max(4096, min(65536, b.cfg.Channels.CatchUpLimit*max(1, mapped)))
}

func (b *Bridge) cacheDiscordRefLocked(ergoMsgID string, ref discordMessageRef) {
	if ergoMsgID == "" || ref.source == "" || !validDiscordID(ref.messageID) {
		return
	}
	key := discordRefKey{source: ircCasefold(ref.source), messageID: ref.messageID}
	if old := b.discordRefs[ergoMsgID]; old.messageID != "" {
		oldKey := discordRefKey{source: ircCasefold(old.source), messageID: old.messageID}
		if oldKey == key {
			return
		}
	}
	b.discordRefs[ergoMsgID] = ref
	ids, exists := b.ergoByDiscord[key]
	if !exists {
		b.discordRefOrder = append(b.discordRefOrder, key)
	}
	for _, existing := range ids {
		if existing == ergoMsgID {
			return
		}
	}
	b.ergoByDiscord[key] = append(ids, ergoMsgID)
	for len(b.discordRefOrder) > b.discordRefLimitLocked() {
		oldest := b.discordRefOrder[0]
		b.discordRefOrder = b.discordRefOrder[1:]
		for _, id := range b.ergoByDiscord[oldest] {
			delete(b.discordRefs, id)
		}
		delete(b.ergoByDiscord, oldest)
	}
}

func (b *Bridge) onRDirCDTopic(conn *ircevent.Connection, msg ircmsg.Message) {
	if len(msg.Params) < 2 {
		return
	}
	b.mu.RLock()
	destination := b.sourceToDest[ircCasefold(msg.Params[0])]
	current := b.rdircd == conn
	b.mu.RUnlock()
	if current && destination != "" {
		b.setErgoTopic(destination, msg.Params[1])
	}
}

func (b *Bridge) setErgoTopic(destination, topic string) {
	b.mu.RLock()
	conn := b.ergo
	ready := b.ergoRegistered && b.operReady
	b.mu.RUnlock()
	if conn == nil || !ready {
		return
	}
	topic = truncateUTF8(ircfmt.Strip(topic), 390)
	if err := conn.Send("TOPIC", destination, topic); err != nil {
		b.log.Warn("failed to mirror channel topic", "channel", destination, "error", err)
	}
}

func (b *Bridge) onErgoJoin(conn *ircevent.Connection, msg ircmsg.Message) {
	account := msg.Nick()
	if len(msg.Params) > 1 && msg.Params[1] != "" && msg.Params[1] != "*" {
		account = msg.Params[1]
	}
	for _, owner := range b.cfg.Ergo.OwnerAccounts {
		if ircCasefold(account) == ircCasefold(owner) {
			b.mu.Lock()
			b.ownerNicks[ircCasefold(owner)] = msg.Nick()
			b.mu.Unlock()
			b.scheduleOwnerAutoJoin(conn, owner, msg.Nick())
			return
		}
	}
}

func (b *Bridge) scheduleOwnerAutoJoin(conn *ircevent.Connection, account, nick string) {
	key := ircCasefold(account)
	b.mu.Lock()
	if b.ergo != conn || b.autoJoinPending[key] {
		b.mu.Unlock()
		return
	}
	b.autoJoinPending[key] = true
	b.mu.Unlock()

	time.AfterFunc(250*time.Millisecond, func() {
		b.mu.Lock()
		if b.ergo != conn || !b.ergoRegistered || !b.operReady {
			delete(b.autoJoinPending, key)
			b.mu.Unlock()
			return
		}
		destinations := make([]string, 0, len(b.destToSource))
		for destination := range b.destToSource {
			destinations = append(destinations, destination)
		}
		b.mu.Unlock()
		for _, destination := range destinations {
			if err := conn.Send("SAJOIN", nick, destination); err != nil {
				b.log.Warn("failed to retry owner autojoin", "account", account, "channel", destination, "error", err)
			}
		}
		time.AfterFunc(time.Second, func() {
			b.mu.Lock()
			delete(b.autoJoinPending, key)
			b.mu.Unlock()
		})
	})
}

func (b *Bridge) autoJoinOwners(conn *ircevent.Connection, destination string) {
	b.mu.RLock()
	nicks := make(map[string]string, len(b.cfg.Ergo.OwnerAccounts))
	for _, account := range b.cfg.Ergo.OwnerAccounts {
		nicks[account] = b.ownerNicks[ircCasefold(account)]
	}
	b.mu.RUnlock()
	for account, nick := range nicks {
		if nick == "" {
			nick = account
		}
		if err := conn.Send("SAJOIN", nick, destination); err != nil {
			b.log.Warn("failed to autojoin owner", "account", account, "channel", destination, "error", err)
		}
	}
}

func (b *Bridge) onRDirCDMessage(ctx context.Context, conn *ircevent.Connection, msg ircmsg.Message, notice bool) {
	if ctx.Err() != nil || len(msg.Params) < 2 {
		return
	}
	target, text := msg.Params[0], msg.Params[1]
	b.mu.RLock()
	destination := b.sourceToDest[ircCasefold(target)]
	control := b.sourceToDest[ircCasefold("#rdircd.control")]
	current := b.rdircd == conn
	b.mu.RUnlock()
	if !current {
		return
	}
	if destination == "" && ircCasefold(target) == ircCasefold(conn.CurrentNick()) {
		destination = control
	}
	if destination == "" {
		return
	}
	nick := msg.Nick()
	if nick == "" {
		nick = "discord"
	}
	if _, userID := msg.GetTag("+dickord/discord-userid"); validDiscordID(userID) {
		b.mu.Lock()
		b.discordUsers[discordUserKey{source: ircCasefold(target), nick: ircCasefold(nick)}] = userID
		b.mu.Unlock()
	}
	if self, value := msg.GetTag("+dickord/self"); self && value == "1" && len(b.cfg.Ergo.OwnerAccounts) > 0 {
		nick = b.cfg.Ergo.OwnerAccounts[0]
	}
	_, event := msg.GetTag("+dickord/event")
	_, discordMessageID := msg.GetTag("+dickord/discord-msgid")
	_, discordReplyMessageID := msg.GetTag("+dickord/discord-reply-msgid")
	avatarPresent, avatar := msg.GetTag("+dickord/avatar")
	if !validDiscordID(discordReplyMessageID) {
		discordReplyMessageID = ""
	}
	switch event {
	case "delete":
		if validDiscordID(discordMessageID) {
			b.relayRedactionWithRetry(destination, 50, target, discordMessageID)
		}
		return
	case "react-add", "react-remove":
		_, emoji := msg.GetTag("+dickord/emoji")
		if emoji == "" || !validDiscordID(discordMessageID) {
			b.log.Warn("ignored malformed structured Discord reaction", "channel", destination)
			return
		}
		reaction := discordReaction{add: event == "react-add", emoji: emoji}
		b.relayReactionWithRetry(destination, nick, reaction, text, notice, 50, target, discordMessageID)
		return
	case "react-remove-all", "react-remove-emoji":
		// IRC has no native equivalent; preserve these explicit state changes as notices.
		b.relayToErgo(destination, nick, text, true, discordMessageRef{}, "", false)
		return
	}
	_, attachment := msg.GetTag("+dickord/attachment")
	// Legacy fallback supports rollback to an older rdircd image.
	if attachment == "" && isDiscordDeletion(text) && validDiscordID(discordMessageID) {
		b.relayRedactionWithRetry(destination, 50, target, discordMessageID)
		return
	}
	if reaction, ok := parseDiscordReaction(text); attachment == "" && ok {
		b.relayReactionWithRetry(destination, nick, reaction, text, notice, 50, target, discordMessageID)
		return
	}
	if attachment != "" && validDiscordID(discordMessageID) && destination != control {
		b.mu.RLock()
		ergo := b.ergo
		ready := b.ergoRegistered && b.operReady
		b.mu.RUnlock()
		if ergo == nil || !ready {
			return
		}
		support := ergo.ISupport()
		endpoint := support["FILEHOST"]
		if endpoint == "" {
			endpoint = support["draft/FILEHOST"]
		}
		if endpoint == "" {
			endpoint = support["soju.im/FILEHOST"]
		}
		if endpoint != "" {
			uploaded, err := b.uploads.Upload(ctx, endpoint, attachment)
			if ctx.Err() != nil {
				return
			}
			b.mu.RLock()
			current := b.ergo == ergo && b.rdircd == conn && b.ergoRegistered && b.operReady
			b.mu.RUnlock()
			if !current {
				return
			}
			rnick := relayNick(nick, nickLength(support))
			limit := min(400-len(rnick)-3, 510-len("RELAYMSG ")-len(destination)-1-len(rnick)-2)
			if err != nil {
				b.log.Warn("attachment upload failed; retaining Discord link", "error", err)
			} else if len(uploaded) > limit {
				b.log.Warn("uploaded attachment URL exceeds IRC line limit; retaining Discord link")
			} else {
				text = uploaded
			}
		}
	}
	b.relayToErgo(destination, nick, text, notice, discordMessageRef{
		source: target, messageID: discordMessageID, replyMessageID: discordReplyMessageID,
	}, avatar, avatarPresent)
}

func validDiscordID(value string) bool {
	_, err := strconv.ParseUint(value, 10, 64)
	return value != "" && err == nil
}

func (b *Bridge) relayReactionWithRetry(destination, nick string, reaction discordReaction, fallbackText string, notice bool, attempts int, source, discordMessageID string) {
	var ergoMsgID string
	if discordMessageID != "" {
		ref := discordMessageRef{source: source, messageID: discordMessageID}
		if b.consumePendingReactionRef(ref, reaction) {
			return
		}
		b.mu.RLock()
		ids := b.ergoByDiscord[discordRefKey{source: ircCasefold(source), messageID: discordMessageID}]
		if len(ids) > 0 {
			ergoMsgID = ids[0]
		}
		b.mu.RUnlock()
	}
	if ergoMsgID == "" && reaction.originalNick != "" && reaction.originalText != "" {
		ergoMsgID = b.lookupMessageRef(destination, reaction.originalNick, reaction.originalText)
		if ergoMsgID != "" && b.consumePendingReaction(ergoMsgID, reaction) {
			return
		}
	}
	if ergoMsgID != "" {
		if !b.sendNativeReaction(destination, reaction, ergoMsgID) {
			b.log.Warn("native Discord reaction could not be sent", "channel", destination)
		}
		return
	}
	if attempts <= 0 {
		b.log.Warn("Discord reaction target is not cached; text fallback suppressed", "channel", destination)
		return
	}
	time.AfterFunc(100*time.Millisecond, func() {
		b.relayReactionWithRetry(destination, nick, reaction, fallbackText, notice, attempts-1, source, discordMessageID)
	})
}

func (b *Bridge) relayToErgo(destination, nick, text string, notice bool, discord discordMessageRef, avatar string, avatarPresent bool) {
	b.mu.RLock()
	conn := b.ergo
	ready := b.ergoRegistered && b.operReady
	relay := b.relayReady
	var replyMsgID string
	if discord.replyMessageID != "" {
		ids := b.ergoByDiscord[discordRefKey{
			source: ircCasefold(discord.source), messageID: discord.replyMessageID,
		}]
		if len(ids) > 0 {
			replyMsgID = ids[0]
		}
	}
	descriptor := b.descriptors[ircCasefold(discord.source)]
	b.mu.RUnlock()
	if conn == nil || !ready {
		return
	}

	rnick := relayNick(nick, nickLength(conn.ISupport()))
	if relay {
		maxPayload := 510 - len("RELAYMSG ") - len(destination) - 1 - len(rnick) - 2
		if maxPayload > 400 {
			maxPayload = 400
		}
		_, labeled := conn.AcknowledgedCaps()["labeled-response"]
		for index, line := range splitUTF8(text, maxPayload) {
			nonce := b.addPendingRelay(destination, nick, line, discord)
			tags := map[string]string{"+dickord/nonce": nonce}
			if descriptor != "" {
				tags["+dickord/channel"] = descriptor
			}
			if index == 0 && replyMsgID != "" {
				tags["+reply"] = replyMsgID
			}
			if avatarPresent {
				tags["+dickord/avatar"] = avatar
			}
			var err error
			for {
				if labeled {
					err = conn.SendWithLabel(func(response *ircevent.Batch) {
						if response == nil {
							b.log.Warn("RELAYMSG response timed out; not retrying to avoid duplicates", "channel", destination)
							return
						}
						if !batchHasError(response) {
							return
						}
						b.removePendingRelay(nonce)
						b.disableRelay(conn)
						b.sendPrefixed(conn, destination, rnick, line, notice)
					}, tags, "RELAYMSG", destination, rnick, line)
				} else {
					err = conn.SendWithTags(tags, "RELAYMSG", destination, rnick, line)
				}
				if err != ircmsg.ErrorTagsTooLong || tags["+dickord/channel"] == "" {
					break
				}
				// The serializer checks the full client-tag budget, including the
				// actual label, before queueing bytes. Retry only without the descriptor.
				delete(tags, "+dickord/channel")
			}
			if err == nil {
				continue
			}
			b.removePendingRelay(nonce)
			b.log.Warn("RELAYMSG send failed; using text prefix", "error", err, "channel", destination)
			b.sendPrefixed(conn, destination, rnick, line, notice)
		}
		return
	}
	for _, line := range splitUTF8(text, 400-len(rnick)-3) {
		b.sendPrefixed(conn, destination, rnick, line, notice)
	}
}

func (b *Bridge) addPendingRelay(destination, nick, text string, discord discordMessageRef) string {
	b.mu.Lock()
	b.nextRelayNonce++
	nonce := strconv.FormatUint(b.nextRelayNonce, 36)
	b.pendingRelays[nonce] = pendingRelay{destination: destination, nick: nick, text: text, discord: discord}
	b.mu.Unlock()
	time.AfterFunc(2*time.Minute, func() { b.removePendingRelay(nonce) })
	return nonce
}

func (b *Bridge) removePendingRelay(nonce string) {
	b.mu.Lock()
	delete(b.pendingRelays, nonce)
	b.mu.Unlock()
}

func (b *Bridge) captureRelayEcho(conn *ircevent.Connection, msg ircmsg.Message) bool {
	present, nonce := msg.GetTag("+dickord/nonce")
	if !present {
		return false
	}
	_, msgID := msg.GetTag("msgid")
	b.mu.Lock()
	pending, ok := b.pendingRelays[nonce]
	delete(b.pendingRelays, nonce)
	if ok && msgID != "" && b.ergo == conn {
		key := messageRefKey{ircCasefold(pending.destination), ircCasefold(pending.nick), pending.text}
		if _, exists := b.messageRefs[key]; !exists {
			b.messageRefOrder = append(b.messageRefOrder, key)
		}
		b.messageRefs[key] = msgID
		b.cacheDiscordRefLocked(msgID, pending.discord)
		if len(b.messageRefOrder) > 4096 {
			oldest := b.messageRefOrder[0]
			b.messageRefOrder = b.messageRefOrder[1:]
			delete(b.messageRefs, oldest)
		}
	}
	b.mu.Unlock()
	return true
}

func (b *Bridge) lookupMessageRef(destination, nick, text string) string {
	destination, nick = ircCasefold(destination), ircCasefold(nick)
	b.mu.RLock()
	defer b.mu.RUnlock()
	if msgID := b.messageRefs[messageRefKey{destination, nick, text}]; msgID != "" {
		return msgID
	}
	for i := len(b.messageRefOrder) - 1; i >= 0; i-- {
		key := b.messageRefOrder[i]
		if key.destination == destination && key.nick == nick && (strings.HasSuffix(key.text, text) || strings.HasSuffix(text, key.text)) {
			return b.messageRefs[key]
		}
	}
	return ""
}

func reactionEmojiKey(emoji string) string {
	if strings.HasPrefix(emoji, ":") && strings.HasSuffix(emoji, ":") {
		return strings.ToLower(emoji)
	}
	return emoji
}

func (b *Bridge) markPendingReaction(ref discordMessageRef, emoji string, add bool) (pendingDiscordReaction, bool) {
	key := pendingDiscordReaction{source: ircCasefold(ref.source), messageID: ref.messageID, emoji: reactionEmojiKey(emoji), add: add}
	b.mu.Lock()
	if expires, exists := b.pendingReactions[key]; exists && time.Now().Before(expires) {
		b.mu.Unlock()
		return key, false
	}
	b.pendingReactions[key] = time.Now().Add(15 * time.Second)
	b.mu.Unlock()
	time.AfterFunc(15*time.Second, func() {
		b.mu.Lock()
		delete(b.pendingReactions, key)
		b.mu.Unlock()
	})
	return key, true
}

func (b *Bridge) removePendingReaction(key pendingDiscordReaction) {
	b.mu.Lock()
	delete(b.pendingReactions, key)
	b.mu.Unlock()
}

func (b *Bridge) consumePendingReaction(ergoMsgID string, reaction discordReaction) bool {
	b.mu.RLock()
	ref := b.discordRefs[ergoMsgID]
	b.mu.RUnlock()
	return b.consumePendingReactionRef(ref, reaction)
}

func (b *Bridge) consumePendingReactionRef(ref discordMessageRef, reaction discordReaction) bool {
	if ref.messageID == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := pendingDiscordReaction{source: ircCasefold(ref.source), messageID: ref.messageID, emoji: reactionEmojiKey(reaction.emoji), add: reaction.add}
	expires, ok := b.pendingReactions[key]
	if !ok || time.Now().After(expires) {
		delete(b.pendingReactions, key)
		return false
	}
	delete(b.pendingReactions, key)
	return true
}

func (b *Bridge) markPendingDeletion(ref discordMessageRef) (discordRefKey, bool) {
	key := discordRefKey{source: ircCasefold(ref.source), messageID: ref.messageID}
	b.mu.Lock()
	if expires, exists := b.pendingDeletions[key]; exists && time.Now().Before(expires) {
		b.mu.Unlock()
		return key, false
	}
	b.pendingDeletions[key] = time.Now().Add(15 * time.Second)
	b.mu.Unlock()
	time.AfterFunc(15*time.Second, func() {
		b.mu.Lock()
		delete(b.pendingDeletions, key)
		b.mu.Unlock()
	})
	return key, true
}

func (b *Bridge) removePendingDeletion(key discordRefKey) {
	b.mu.Lock()
	delete(b.pendingDeletions, key)
	b.mu.Unlock()
}

func (b *Bridge) consumePendingDeletion(source, messageID string) bool {
	key := discordRefKey{source: ircCasefold(source), messageID: messageID}
	b.mu.Lock()
	defer b.mu.Unlock()
	expires, ok := b.pendingDeletions[key]
	if !ok || time.Now().After(expires) {
		delete(b.pendingDeletions, key)
		return false
	}
	delete(b.pendingDeletions, key)
	return true
}

func (b *Bridge) disableRelay(conn *ircevent.Connection) {
	b.mu.Lock()
	if b.ergo == conn {
		b.relayReady = false
	}
	b.mu.Unlock()
	b.log.Warn("RELAYMSG rejected; using text-prefix fallback")
}

func batchHasError(batch *ircevent.Batch) bool {
	if batch == nil {
		return false
	}
	command := strings.ToUpper(batch.Command)
	if command == "FAIL" || command == "ERROR" || len(command) == 3 && (command[0] == '4' || command[0] == '5') {
		return true
	}
	for _, item := range batch.Items {
		if batchHasError(item) {
			return true
		}
	}
	return false
}

func (b *Bridge) sendPrefixed(conn *ircevent.Connection, destination, nick, text string, notice bool) {
	line := "<" + strings.TrimSuffix(nick, "/discord") + "> " + text
	var err error
	if notice {
		err = conn.Notice(destination, line)
	} else {
		err = conn.Privmsg(destination, line)
	}
	if err != nil {
		b.log.Error("fallback relay send failed", "error", err, "channel", destination)
	}
}

func isDiscordDeletion(text string) bool {
	return strings.Contains(text, "message was deleted")
}

func (b *Bridge) relayRedactionWithRetry(destination string, attempts int, source, discordMessageID string) {
	if b.consumePendingDeletion(source, discordMessageID) {
		return
	}
	b.mu.RLock()
	ergoMsgIDs := append([]string(nil), b.ergoByDiscord[discordRefKey{source: ircCasefold(source), messageID: discordMessageID}]...)
	b.mu.RUnlock()
	if len(ergoMsgIDs) > 0 {
		for _, ergoMsgID := range ergoMsgIDs {
			if !b.sendNativeRedaction(destination, ergoMsgID) {
				b.log.Warn("native redaction could not be sent", "channel", destination, "msgid", ergoMsgID)
			}
		}
		return
	}
	if attempts <= 0 {
		b.log.Warn("deletion target is not cached; text fallback suppressed", "channel", destination)
		return
	}
	time.AfterFunc(100*time.Millisecond, func() {
		b.relayRedactionWithRetry(destination, attempts-1, source, discordMessageID)
	})
}

func (b *Bridge) sendNativeRedaction(destination, ergoMsgID string) bool {
	b.mu.RLock()
	conn := b.ergo
	ready := b.ergoRegistered && b.operReady
	b.mu.RUnlock()
	if conn == nil || !ready {
		return false
	}
	if _, supported := conn.AcknowledgedCaps()["draft/message-redaction"]; !supported {
		return false
	}
	if _, labeled := conn.AcknowledgedCaps()["labeled-response"]; labeled {
		err := conn.SendWithLabel(func(response *ircevent.Batch) {
			if response != nil && batchHasError(response) {
				b.log.Warn("Ergo rejected native redaction", "channel", destination)
			}
		}, nil, "REDACT", destination, ergoMsgID, "deleted on Discord")
		return err == nil
	}
	return conn.Send("REDACT", destination, ergoMsgID, "deleted on Discord") == nil
}

func parseDiscordReaction(text string) (discordReaction, bool) {
	index := strings.Index(text, "reacts: ")
	if index < 0 {
		return discordReaction{}, false
	}
	reactionText, reference, hasReference := strings.Cut(text[index+len("reacts: "):], " :: ")
	fields := strings.Fields(reactionText)
	if len(fields) == 0 || len(fields[0]) < 2 || fields[0][0] != '+' && fields[0][0] != '-' {
		return discordReaction{}, false
	}
	reaction := discordReaction{add: fields[0][0] == '+', emoji: fields[0][1:]}
	if reaction.emoji == "" || reaction.emoji == "all" {
		return discordReaction{}, false
	}
	if !hasReference || !strings.HasPrefix(reference, "[") {
		return reaction, true
	}
	endTimestamp := strings.Index(reference, "] <")
	if endTimestamp < 0 {
		return reaction, true
	}
	reference = reference[endTimestamp+3:]
	endNick := strings.Index(reference, "> ")
	if endNick < 0 {
		return reaction, true
	}
	reaction.originalNick = reference[:endNick]
	reaction.originalText = reference[endNick+2:]
	return reaction, true
}

func (b *Bridge) sendNativeReaction(destination string, reaction discordReaction, msgID string) bool {
	b.mu.RLock()
	conn := b.ergo
	ready := b.ergoRegistered && b.operReady && b.relayReady
	b.mu.RUnlock()
	if conn == nil || !ready {
		return false
	}
	tag := "+react"
	if !reaction.add {
		tag = "+unreact"
	}
	tags := map[string]string{"+reply": msgID, tag: reaction.emoji}
	if _, labeled := conn.AcknowledgedCaps()["labeled-response"]; labeled {
		err := conn.SendWithLabel(func(response *ircevent.Batch) {
			if response != nil && batchHasError(response) {
				b.log.Warn("Ergo rejected native Discord reaction", "channel", destination)
			}
		}, tags, "TAGMSG", destination)
		return err == nil
	}
	return conn.SendWithTags(tags, "TAGMSG", destination) == nil
}

func reactionTag(msg ircmsg.Message) (emoji string, add bool, ok bool) {
	for _, candidate := range []string{"+draft/react", "+react"} {
		if present, value := msg.GetTag(candidate); present && value != "" {
			if ok {
				return "", false, false
			}
			emoji, add, ok = value, true, true
		}
	}
	for _, candidate := range []string{"+draft/unreact", "+unreact"} {
		if present, value := msg.GetTag(candidate); present && value != "" {
			if ok {
				return "", false, false
			}
			emoji, add, ok = value, false, true
		}
	}
	return
}

func reactionTagsPresent(msg ircmsg.Message) bool {
	for _, tag := range []string{"+draft/react", "+react", "+draft/unreact", "+unreact"} {
		if present, _ := msg.GetTag(tag); present {
			return true
		}
	}
	return false
}

func (b *Bridge) onErgoReaction(conn *ircevent.Connection, msg ircmsg.Message) bool {
	if !reactionTagsPresent(msg) {
		return false
	}
	if len(msg.Params) < 1 || isPlayback(msg) {
		return true
	}
	if _, _, authorized := authorizedMessage(msg, b.cfg.OwnerAccountsSet); !authorized {
		return true
	}
	emoji, add, ok := reactionTag(msg)
	present, replyID := msg.GetTag("+reply")
	destination := msg.Params[0]
	if !ok || !present || replyID == "" {
		b.notifyErgo(destination, "Malformed IRC reaction; reaction not sent")
		return true
	}

	b.forwardErgoReactionWithRetry(conn, destination, replyID, emoji, add, 50)
	return true
}

func (b *Bridge) forwardErgoReactionWithRetry(conn *ircevent.Connection, destination, replyID, emoji string, add bool, attempts int) {
	b.mu.RLock()
	ref := b.discordRefs[replyID]
	source := b.destToSource[ircCasefold(destination)]
	rdircd := b.rdircd
	ready := b.rdircdReady && rdircd != nil && b.ergo == conn && b.ergoRegistered && b.operReady
	b.mu.RUnlock()
	if !ready {
		b.notifyErgo(destination, "Discord bridge unavailable; reaction not sent")
		return
	}
	if ref.messageID == "" {
		if attempts <= 0 {
			b.notifyErgo(destination, "Reaction target is not a cached Discord message; reaction not sent")
			return
		}
		time.AfterFunc(100*time.Millisecond, func() {
			b.forwardErgoReactionWithRetry(conn, destination, replyID, emoji, add, attempts-1)
		})
		return
	}
	if ircCasefold(ref.source) != ircCasefold(source) {
		b.notifyErgo(destination, "Reaction target belongs to a different Discord channel; reaction not sent")
		return
	}
	tag := "+draft/react"
	if !add {
		tag = "+draft/unreact"
	}
	pending, fresh := b.markPendingReaction(ref, emoji, add)
	if !fresh {
		return
	}
	if err := rdircd.SendWithTags(map[string]string{"+reply": ref.messageID, tag: emoji}, "TAGMSG", ref.source); err != nil {
		b.removePendingReaction(pending)
		b.notifyErgo(destination, "Discord bridge unavailable; reaction not sent")
	}
}

func (b *Bridge) onErgoChannelSnapshotRequest(conn *ircevent.Connection, msg ircmsg.Message) bool {
	present, version := msg.GetTag("+dickord/channel-request")
	if !present {
		return false
	}
	if version != "1" || len(msg.Params) != 1 || isPlayback(msg) {
		return true
	}
	if _, _, authorized := authorizedMessage(msg, b.cfg.OwnerAccountsSet); !authorized {
		return true
	}

	b.mu.Lock()
	source := b.destToSource[ircCasefold(msg.Params[0])]
	rdircd := b.rdircd
	ready := b.ergo == conn && b.ergoRegistered && b.operReady && b.rdircdReady && rdircd != nil
	if !ready || ircCasefold(source) != ircCasefold("#rdircd.control") {
		b.mu.Unlock()
		return true
	}
	seeds := make(map[string]string, len(b.descriptors))
	for source, descriptor := range b.descriptors {
		if destination := b.sourceToDest[source]; destination != "" {
			seeds[destination] = descriptor
		}
	}
	refresh := !b.rdircdRefreshing
	b.rdircdRefreshing = true
	b.mu.Unlock()

	for destination, descriptor := range seeds {
		if err := conn.SendWithTags(map[string]string{"+dickord/channel": descriptor}, "TAGMSG", destination); err != nil {
			b.log.Warn("failed to resend channel descriptor tag", "channel", destination, "error", err)
		}
	}
	if !refresh {
		return true
	}
	if err := rdircd.Send("LIST"); err != nil {
		b.mu.Lock()
		if b.rdircd == rdircd {
			b.rdircdRefreshing = false
		}
		b.mu.Unlock()
		b.log.Warn("failed to refresh rdircd channel discovery", "error", err)
	}
	return true
}

func (b *Bridge) onErgoTagMessage(conn *ircevent.Connection, msg ircmsg.Message) {
	if b.onErgoChannelSnapshotRequest(conn, msg) {
		return
	}
	if b.onErgoReaction(conn, msg) {
		return
	}
	present, state := msg.GetTag("+typing")
	if !present || len(msg.Params) != 1 || isPlayback(msg) {
		return
	}
	switch state {
	case "active", "paused", "done":
	default:
		return
	}
	if _, _, authorized := authorizedMessage(msg, b.cfg.OwnerAccountsSet); !authorized {
		return
	}

	destination := msg.Params[0]
	b.mu.RLock()
	source := b.destToSource[ircCasefold(destination)]
	rdircd := b.rdircd
	ready := b.rdircdReady && rdircd != nil && b.ergo == conn && b.ergoRegistered && b.operReady
	b.mu.RUnlock()
	if !ready || source == "" {
		return
	}
	// rdircd owns the privacy opt-in and interprets paused/done as stop states.
	_ = rdircd.SendWithTags(map[string]string{"+typing": state}, "TAGMSG", source)
}

func (b *Bridge) onErgoRedact(conn *ircevent.Connection, msg ircmsg.Message) {
	if len(msg.Params) < 2 || isPlayback(msg) || msg.Nick() == conn.CurrentNick() {
		return
	}
	if _, _, authorized := authorizedMessage(msg, b.cfg.OwnerAccountsSet); !authorized {
		return
	}
	b.forwardErgoRedactionWithRetry(conn, msg.Params[0], msg.Params[1], 50)
}

func (b *Bridge) forwardErgoRedactionWithRetry(conn *ircevent.Connection, destination, ergoMsgID string, attempts int) {
	b.mu.RLock()
	ref := b.discordRefs[ergoMsgID]
	source := b.destToSource[ircCasefold(destination)]
	rdircd := b.rdircd
	ready := b.rdircdReady && rdircd != nil && b.ergo == conn && b.ergoRegistered && b.operReady
	b.mu.RUnlock()
	if !ready {
		b.notifyErgo(destination, "Discord bridge unavailable; deletion not sent")
		return
	}
	if ref.messageID == "" {
		if attempts <= 0 {
			b.notifyErgo(destination, "Redaction target is not a cached Discord message; deletion not sent")
			return
		}
		time.AfterFunc(100*time.Millisecond, func() {
			b.forwardErgoRedactionWithRetry(conn, destination, ergoMsgID, attempts-1)
		})
		return
	}
	if ircCasefold(ref.source) != ircCasefold(source) {
		b.notifyErgo(destination, "Redaction target belongs to a different Discord channel; deletion not sent")
		return
	}
	pending, fresh := b.markPendingDeletion(ref)
	if !fresh {
		return
	}
	if err := rdircd.Send("REDACT", ref.source, ref.messageID); err != nil {
		b.removePendingDeletion(pending)
		b.notifyErgo(destination, "Discord bridge unavailable; deletion not sent")
	}
}

func (b *Bridge) onErgoMessage(conn *ircevent.Connection, msg ircmsg.Message) {
	if b.captureRelayEcho(conn, msg) {
		return
	}
	if msg.Nick() == conn.CurrentNick() {
		return
	}
	if present, _ := msg.GetTag("draft/relaymsg"); present {
		return
	}
	if present, _ := msg.GetTag("relaymsg"); present {
		return
	}
	if b.onErgoReaction(conn, msg) {
		return
	}
	if len(msg.Params) < 2 || isPlayback(msg) {
		return
	}
	account, reason, authorized := authorizedMessage(msg, b.cfg.OwnerAccountsSet)
	if !authorized {
		b.log.Warn("ignored Ergo message", "reason", reason, "account", account, "channel", msg.Params[0])
		return
	}

	destination, text := msg.Params[0], ircfmt.Strip(msg.Params[1])
	_, replyID := msg.GetTag("+reply")
	b.mu.RLock()
	source := b.destToSource[ircCasefold(destination)]
	rdircd := b.rdircd
	ready := b.rdircdReady && rdircd != nil && b.ergo == conn && b.ergoRegistered && b.operReady
	replyRef := b.discordRefs[replyID]
	b.mu.RUnlock()
	discordReplyID := ""
	if replyRef.messageID != "" && ircCasefold(replyRef.source) == ircCasefold(source) {
		discordReplyID = replyRef.messageID
	}
	if source == "" {
		return
	}
	if !ready {
		b.notifyErgo(destination, "Discord bridge unavailable; message not sent")
		return
	}

	if ircCasefold(strings.TrimPrefix(source, "#")) != "rdircd.control" {
		if args, ok := topicCommand(text); ok {
			var err error
			if args == "" {
				err = rdircd.Send("TOPIC", source)
			} else {
				err = rdircd.Send("TOPIC", source, args)
			}
			if err != nil {
				b.notifyErgo(destination, "Discord bridge unavailable; topic command not sent")
			}
			return
		}
	}

	if strings.TrimSpace(text) == "" {
		return
	}
	_, ergoMsgID := msg.GetTag("msgid")
	voiceDurationPresent, voiceDuration := msg.GetTag("+dickord/voice-duration")
	voiceWaveformPresent, voiceWaveform := msg.GetTag("+dickord/voice-waveform")
	voiceRequested := voiceDurationPresent || voiceWaveformPresent
	uploadText := text
	if present, version := msg.GetTag("+trevarj.github.io/audio"); !present || version == "1" {
		if voiceURL, ok := motdVoiceUploadURL(text); ok {
			uploadText = voiceURL
		}
	}
	uploadURL, urlErr := attachmentHTTPSURL(uploadText)
	support := conn.ISupport()
	endpoint := support["FILEHOST"]
	if endpoint == "" {
		endpoint = support["draft/FILEHOST"]
	}
	if endpoint == "" {
		endpoint = support["soju.im/FILEHOST"]
	}
	filehostURL, endpointErr := attachmentHTTPSURL(endpoint)
	uploadRequested := len(uploadText) <= maxAttachmentURLLength && urlErr == nil && endpointErr == nil &&
		sameHTTPSOrigin(uploadURL, filehostURL)
	if voiceRequested {
		duration, err := strconv.ParseFloat(voiceDuration, 64)
		waveform, waveformErr := base64.StdEncoding.Strict().DecodeString(voiceWaveform)
		if !voiceDurationPresent || !voiceWaveformPresent || err != nil || duration <= 0 ||
			math.IsNaN(duration) || math.IsInf(duration, 0) || waveformErr != nil ||
			len(waveform) == 0 || len(waveform) > 256 || !uploadRequested {
			b.notifyErgo(destination, "Invalid voice message; expected a FILEHOST URL, positive duration, and 1-256 waveform bytes")
			return
		}
	}
	if uploadRequested {
		tags := map[string]string{"+dickord/upload": "1"}
		if voiceRequested {
			tags["+dickord/voice-duration"] = voiceDuration
			tags["+dickord/voice-waveform"] = voiceWaveform
		}
		if ergoMsgID != "" {
			tags["+dickord/ergo-msgid"] = ergoMsgID
		}
		if discordReplyID != "" {
			tags["+dickord/discord-reply-msgid"] = discordReplyID
		}
		if err := rdircd.SendWithTags(tags, "PRIVMSG", source, uploadURL.String()); err != nil {
			b.notifyErgo(destination, "Discord bridge unavailable; media not sent")
		}
		return
	}
	text = b.translateDiscordMentions(source, text)
	for index, line := range splitUTF8(text, 380) {
		tags := make(map[string]string, 2)
		if ergoMsgID != "" {
			tags["+dickord/ergo-msgid"] = ergoMsgID
		}
		// One logical reply can split into many Discord posts; only the first keeps the relation.
		if index == 0 && discordReplyID != "" {
			tags["+dickord/discord-reply-msgid"] = discordReplyID
		}
		if err := rdircd.SendWithTags(tags, "PRIVMSG", source, line); err != nil {
			b.notifyErgo(destination, "Discord bridge unavailable; message not sent")
			return
		}
	}
}

func (b *Bridge) notifyErgo(channel, text string) {
	b.mu.RLock()
	conn := b.ergo
	ready := b.ergoRegistered
	b.mu.RUnlock()
	if conn != nil && ready {
		_ = conn.Notice(channel, text)
	}
}

func authorizedMessage(msg ircmsg.Message, owners map[string]struct{}) (account, reason string, authorized bool) {
	if present, _ := msg.GetTag("draft/relaymsg"); present {
		return "", "relay-loop", false
	}
	if present, _ := msg.GetTag("relaymsg"); present {
		return "", "relay-loop", false
	}
	present, account := msg.GetTag("account")
	if !present || account == "" || account == "*" {
		return account, "missing-account-tag", false
	}
	if _, ok := owners[ircCasefold(account)]; !ok {
		return account, "unauthorized-account", false
	}
	return account, "", true
}

func (b *Bridge) translateDiscordMentions(source, text string) string {
	text = b.translateRelayAddress(source, text)
	source = ircCasefold(source)
	b.mu.RLock()
	defer b.mu.RUnlock()

	var out strings.Builder
	last := 0
	for i := 0; i < len(text); i++ {
		if text[i] != '@' || i > 0 && !strings.ContainsRune(" \t\r\n", rune(text[i-1])) {
			continue
		}
		end := i + 1
		for end < len(text) && !strings.ContainsRune(" \t\r\n,;@+!?:()", rune(text[end])) {
			end++
		}
		if end == i+1 {
			continue
		}
		nick := text[i+1 : end]
		userID := b.discordUsers[discordUserKey{source: source, nick: ircCasefold(nick)}]
		if !validDiscordID(userID) {
			continue
		}
		out.WriteString(text[last:i])
		out.WriteString("<@" + userID + ">")
		last = end
		i = end - 1
	}
	if last == 0 {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}

func (b *Bridge) translateRelayAddress(source, text string) string {
	nick, rest, found := strings.Cut(text, "/discord:")
	if !found || nick == "" || strings.ContainsAny(nick, " \t") {
		return text
	}
	b.mu.RLock()
	userID := b.discordUsers[discordUserKey{source: ircCasefold(source), nick: ircCasefold(nick)}]
	b.mu.RUnlock()
	if validDiscordID(userID) {
		return "<@" + userID + "> " + strings.TrimLeft(rest, " \t")
	}
	return "@" + nick + " " + strings.TrimLeft(rest, " \t")
}

func topicCommand(text string) (string, bool) {
	if text == "!topic" {
		return "", true
	}
	if strings.HasPrefix(text, "!topic ") {
		return strings.TrimSpace(strings.TrimPrefix(text, "!topic ")), true
	}
	return "", false
}

func isPlayback(msg ircmsg.Message) bool {
	for _, tag := range []string{"batch", "znc.in/playback", "draft/chathistory"} {
		if present, _ := msg.GetTag(tag); present {
			return true
		}
	}
	return false
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextDelay(current, maximum time.Duration) time.Duration {
	next := current * 2
	if next > maximum || next < current {
		return maximum
	}
	return next
}

type slogWriter struct {
	logger *slog.Logger
	leg    string
}

func (w slogWriter) Write(p []byte) (int, error) {
	message := strings.TrimSpace(string(p))
	if message != "" {
		w.logger.Warn("IRC client", "leg", w.leg, "message", message)
	}
	return len(p), nil
}
