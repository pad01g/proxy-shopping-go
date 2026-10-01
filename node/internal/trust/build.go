package trust

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/nbd-wtf/go-nostr"
)

func sign(secret string, kind int, tags nostr.Tags, content any, createdAt nostr.Timestamp) (*nostr.Event, error) {
	var c string
	switch v := content.(type) {
	case string:
		c = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal content of kind %d: %w", kind, err)
		}
		c = string(data)
	}
	if createdAt == 0 {
		createdAt = nostr.Now()
	}
	ev := &nostr.Event{Kind: kind, CreatedAt: createdAt, Tags: tags, Content: c}
	if err := ev.Sign(secret); err != nil {
		return nil, fmt.Errorf("sign kind %d: %w", kind, err)
	}
	return ev, nil
}

// NewDelegation signs a kind 30500 delegation of a coordinator to an operator.
func NewDelegation(secret, operator, network string, version int64, revoked bool, note string) (*nostr.Event, error) {
	return NewDelegationWithURLs(secret, operator, network, version, revoked, note, nil)
}

// NewDelegationWithURLs signs a delegation with list_url tags (§2.2): where the operator keeps its list bundle.
func NewDelegationWithURLs(secret, operator, network string, version int64, revoked bool, note string, listURLs []string) (*nostr.Event, error) {
	tags := nostr.Tags{
		{"d", operator}, {"v", strconv.FormatInt(version, 10)}, {"network", network},
		{"p", operator}, {"revoked", strconv.FormatBool(revoked)},
	}
	if len(listURLs) > MaxListURLs {
		return nil, fmt.Errorf("at most %d list_url", MaxListURLs)
	}
	for _, u := range listURLs {
		if !IsHTTPSURL(u) {
			return nil, fmt.Errorf("list_url %q is not an https URL", u)
		}
		tags = append(tags, nostr.Tag{"list_url", u})
	}
	return sign(secret, KindDelegation, tags, map[string]string{"note": note}, 0)
}

// NewList signs a kind 30501 list.
func NewList(secret string, version int64, l *List) (*nostr.Event, error) {
	if l.Network == "" {
		return nil, fmt.Errorf("list needs a network")
	}
	tags := nostr.Tags{{"d", l.Network}, {"v", strconv.FormatInt(version, 10)}, {"network", l.Network}}
	ev, err := sign(secret, KindList, tags, l, 0)
	if err != nil {
		return nil, err
	}
	if _, err := ParseList(ev); err != nil {
		return nil, err
	}
	return ev, nil
}

// NewProfile signs a kind 30502 / 30503 profile.
func NewProfile(secret string, kind int, network string, version int64, content any) (*nostr.Event, error) {
	tags := nostr.Tags{{"d", network}, {"v", strconv.FormatInt(version, 10)}, {"network", network}}
	return sign(secret, kind, tags, content, 0)
}

// NewInboxRelays signs a kind 10050 event. It carries no v tag (NIP-17 clients and relays order 10050 by
// created_at); the version becomes its created_at, so that Version is the same either way. Events with a v tag
// (other implementations) are accepted too.
func NewInboxRelays(secret string, relays []string, version int64) (*nostr.Event, error) {
	tags := nostr.Tags{}
	for _, r := range relays {
		tags = append(tags, nostr.Tag{"relay", r})
	}
	return sign(secret, KindInboxRelays, tags, "", nostr.Timestamp(version))
}
