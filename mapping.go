package main

import (
	"crypto/sha256"
	"fmt"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

var excludedSourcePrefixes = []string{
	"rdircd.debug",
	"rdircd.leftover",
	"rdircd.monitor",
	"rdircd.voice",
}

type channelSelector struct {
	dmPattern string
	guilds    []string
}

func newChannelSelector(cfg ChannelConfig) channelSelector {
	return channelSelector{dmPattern: cfg.DMIncludePattern, guilds: cfg.GuildInclude}
}

func (s channelSelector) selected(source string) bool {
	name := strings.TrimPrefix(source, "#")
	folded := ircCasefold(name)
	if folded == "rdircd.control" {
		return true
	}
	for _, prefix := range excludedSourcePrefixes {
		if folded == prefix || strings.HasPrefix(folded, prefix+".") {
			return false
		}
	}
	if matched, _ := path.Match(s.dmPattern, name); matched {
		return true
	}
	for _, pattern := range s.guilds {
		if matched, _ := path.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

func aliasedSource(source string, aliases map[string]string) string {
	for candidate, alias := range aliases {
		if ircCasefold(strings.TrimPrefix(source, "#")) == ircCasefold(strings.TrimPrefix(candidate, "#")) {
			return "#" + strings.TrimPrefix(alias, "#")
		}
	}
	return source
}

func destinationName(source, prefix string, maxLen int) string {
	if ircCasefold(strings.TrimPrefix(source, "#")) == "rdircd.control" {
		return "#discord.control"
	}
	name := encodeIRCName(strings.TrimPrefix(source, "#"))
	base := "#" + prefix + name
	if len(base) <= maxLen {
		return base
	}
	return hashedName(base, source, maxLen)
}

func collisionName(destination, source string, maxLen int) string {
	return hashedName(destination, source, maxLen)
}

func hashedName(base, source string, maxLen int) string {
	hash := fmt.Sprintf("-%x", sha256.Sum256([]byte(ircCasefold(source))))[:9]
	keep := maxLen - len(hash)
	if keep < 2 {
		keep = 2
	}
	return truncateUTF8(base, keep) + hash
}

// encodeIRCName keeps common readable channel characters and injectively encodes
// everything else. This avoids collisions from replacing different characters
// with the same placeholder.
func encodeIRCName(value string) string {
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._=+", r) {
			out.WriteRune(r)
			continue
		}
		for _, b := range []byte(string(r)) {
			out.WriteByte('_')
			out.WriteString(fmt.Sprintf("%02x", b))
		}
	}
	if out.Len() == 0 {
		return "channel"
	}
	return out.String()
}

func relayNick(nick string, maxLen int) string {
	const suffix = "/discord"
	var out strings.Builder
	lastDash := false
	for _, r := range nick {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_[]{}^|", r)
		if valid {
			out.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			out.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(out.String(), "-")
	if name == "" {
		name = "discord-user"
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "u-" + name
	}
	limit := maxLen - len(suffix)
	if limit < 8 {
		limit = 8
	}
	if len(name) > limit {
		hash := fmt.Sprintf("-%x", sha256.Sum256([]byte(nick)))[:7]
		name = truncateUTF8(name, limit-len(hash)) + hash
	}
	return name + suffix
}

func splitUTF8(text string, maxBytes int) []string {
	if text == "" {
		return nil
	}
	if maxBytes < utf8.UTFMax {
		maxBytes = utf8.UTFMax
	}
	var result []string
	for len(text) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.ValidString(text[:cut]) {
			cut--
		}
		if cut == 0 {
			_, size := utf8.DecodeRuneInString(text)
			cut = size
		}
		if ws := strings.LastIndexAny(text[:cut], " \t"); ws > cut/2 {
			cut = ws
		}
		part := strings.TrimRight(text[:cut], " \t")
		if part != "" {
			result = append(result, part)
		}
		text = strings.TrimLeft(text[cut:], " \t")
	}
	if text != "" {
		result = append(result, text)
	}
	return result
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	for maxBytes > 0 && !utf8.ValidString(value[:maxBytes]) {
		maxBytes--
	}
	return value[:maxBytes]
}

func ircCasefold(value string) string {
	value = strings.ToLower(value)
	return strings.NewReplacer("[", "{", "]", "}", "\\", "|", "^", "~").Replace(value)
}

func channelLength(isupport map[string]string, fallback int) int {
	if value := isupport["CHANNELLEN"]; value != "" {
		if length, err := strconv.Atoi(value); err == nil && length >= 24 {
			return length
		}
	}
	return fallback
}

func nickLength(isupport map[string]string) int {
	if value := isupport["NICKLEN"]; value != "" {
		if length, err := strconv.Atoi(value); err == nil && length >= 16 {
			return length
		}
	}
	return 32
}
