package main

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"time"
)

const emailCacheTTL = 30 * time.Second

// Only these host fields are decoded. Account, label, and file names may contain
// secrets or ambiguous identifiers and must never be treated as email addresses.
type credentialEmail struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index"`
	Email     string `json:"email"`
}

type emailDirectory struct {
	mu        sync.Mutex
	load      func() ([]credentialEmail, error)
	next      time.Time
	emails    map[string]string
	available bool
}

func (d *emailDirectory) setLoader(load func() ([]credentialEmail, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.load, d.next, d.emails, d.available = load, time.Time{}, nil, false
}

func (d *emailDirectory) lookup(now time.Time) (map[string]string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Before(d.next) {
		return d.emails, d.available
	}
	d.next, d.emails, d.available = now.Add(emailCacheTTL), nil, false
	if d.load == nil {
		return nil, false
	}
	files, err := d.load()
	if err != nil {
		return nil, false
	}
	emails := make(map[string]string)
	ambiguous := make(map[string]bool)
	for i, file := range files {
		if i >= maxTrackedRequests {
			break
		}
		email := validEmail(file.Email)
		if email == "" {
			continue
		}
		for _, identity := range []string{"index:" + file.AuthIndex, "id:" + file.ID} {
			if identity == "index:" || identity == "id:" || len(identity) > 4096 {
				continue
			}
			if previous := emails[identity]; previous != "" && previous != email {
				ambiguous[identity] = true
			}
			emails[identity] = email
		}
	}
	for identity := range ambiguous {
		delete(emails, identity)
	}
	d.emails, d.available = emails, true
	return emails, true
}

func validEmail(value string) string {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 254 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Name != "" || address.Address != value {
		return ""
	}
	return value
}

func maskEmail(value string) string {
	at := strings.LastIndexByte(value, '@')
	local := []rune(value[:at])
	visible := 1
	if len(local) > 2 {
		visible = 2
	}
	return string(local[:visible]) + "***" + value[at:]
}

func emailLabel(identities []string, emails map[string]string, mode string) string {
	unique := make(map[string]bool)
	for _, identity := range identities {
		if email := emails[identity]; email != "" {
			unique[email] = true
		}
	}
	values := make([]string, 0, len(unique))
	for email := range unique {
		values = append(values, email)
	}
	sort.Strings(values)
	count := len(values)
	if count > 3 {
		values = values[:3]
	}
	if mode != displayEmail {
		for i := range values {
			values[i] = maskEmail(values[i])
		}
	}
	label := strings.Join(values, "、")
	if count > 3 {
		label += fmt.Sprintf(" 等 %d 个邮箱", count)
	}
	return label
}

func (l *limiter) monitorSnapshot() snapshot {
	result := l.snapshot()
	if result.AccountDisplay == displayFingerprint || len(result.Items) == 0 {
		return result
	}
	// Host callbacks and cache synchronization never hold the admission mutex.
	emails, available := l.directory.lookup(result.ObservedAt)
	result.EmailAvailable = available
	for i := range result.Items {
		result.Items[i].Label = emailLabel(result.Items[i].identities, emails, result.AccountDisplay)
	}
	return result
}

func decodeCredentialEmails(raw json.RawMessage) ([]credentialEmail, error) {
	var result struct {
		Files []credentialEmail `json:"files"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("decode host credential email directory: %w", err)
	}
	return result.Files, nil
}
