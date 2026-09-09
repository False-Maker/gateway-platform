package gateway

import (
	"crypto/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

type cooldownKey struct{ accountID, model string }

type cooldown struct {
	until             time.Time
	credentialVersion int64
}

// bucketState is what the chooser remembers per platform+group bucket when
// it is fed by ReplaceBucket. validUntil mirrors the Redis data-key TTL so a
// bucket whose refresh stopped is excluded instead of serving stale leases.
type bucketState struct {
	epoch      int64
	validUntil time.Time
	ids        map[string]struct{}
}

type Chooser struct {
	mu       sync.RWMutex
	accounts map[string]contracts.Account
	buckets  map[string]bucketState
	owner    map[string]string // account ID -> bucket key
	cooling  map[cooldownKey]cooldown
	failures map[cooldownKey]int
	now      func() time.Time
	epoch    int64
}

func NewChooser(now func() time.Time) *Chooser {
	if now == nil {
		now = time.Now
	}
	return &Chooser{accounts: make(map[string]contracts.Account), buckets: make(map[string]bucketState), owner: make(map[string]string), cooling: make(map[cooldownKey]cooldown), failures: make(map[cooldownKey]int), now: now}
}

func bucketKey(platform, group string) string { return platform + "\x00" + group }

// ReplaceBucket installs one bucket's accounts without disturbing others.
// A changed bucket epoch clears only that bucket's local cooldowns. A zero
// validUntil means "no freshness bound" (tests / static configurations).
func (c *Chooser) ReplaceBucket(platform, group string, accounts []contracts.Account, epoch int64, validUntil time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := bucketKey(platform, group)
	previous, existed := c.buckets[key]
	if existed && epoch > 0 && previous.epoch > 0 && epoch != previous.epoch {
		for cooling := range c.cooling {
			if _, mine := previous.ids[cooling.accountID]; mine {
				delete(c.cooling, cooling)
				delete(c.failures, cooling)
			}
		}
	}
	for id := range previous.ids {
		delete(c.accounts, id)
		delete(c.owner, id)
	}
	next := bucketState{epoch: epoch, validUntil: validUntil, ids: make(map[string]struct{}, len(accounts))}
	for _, account := range accounts {
		if account.Platform != platform || account.Group != group {
			continue
		}
		account.Limits = account.Limits.WithDefaults()
		if owner, taken := c.owner[account.ID]; taken && owner != key {
			continue
		}
		c.accounts[account.ID] = account
		c.owner[account.ID] = key
		next.ids[account.ID] = struct{}{}
		for cooling, value := range c.cooling {
			if cooling.accountID == account.ID && value.credentialVersion != account.Credential.Version {
				delete(c.cooling, cooling)
				delete(c.failures, cooling)
			}
		}
	}
	c.buckets[key] = next
}

// RemoveBucket drops a bucket that control no longer publishes.
func (c *Chooser) RemoveBucket(platform, group string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := bucketKey(platform, group)
	for id := range c.buckets[key].ids {
		delete(c.accounts, id)
		delete(c.owner, id)
	}
	delete(c.buckets, key)
}

// Buckets lists the bucket identities currently installed via ReplaceBucket.
func (c *Chooser) Buckets() [][2]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([][2]string, 0, len(c.buckets))
	for key := range c.buckets {
		platform, group, _ := strings.Cut(key, "\x00")
		out = append(out, [2]string{platform, group})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0]+out[i][1] < out[j][0]+out[j][1] })
	return out
}

func (c *Chooser) Replace(snapshot []contracts.Account) {
	c.ReplaceAtEpoch(snapshot, 0)
}

func (c *Chooser) ReplaceAtEpoch(snapshot []contracts.Account, epoch int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch > 0 && c.epoch > 0 && epoch != c.epoch {
		c.cooling = make(map[cooldownKey]cooldown)
		c.failures = make(map[cooldownKey]int)
	}
	next := make(map[string]contracts.Account, len(snapshot))
	c.buckets = make(map[string]bucketState)
	c.owner = make(map[string]string)
	for _, account := range snapshot {
		account.Limits = account.Limits.WithDefaults()
		next[account.ID] = account
		for key, value := range c.cooling {
			if key.accountID == account.ID && value.credentialVersion != account.Credential.Version {
				delete(c.cooling, key)
				delete(c.failures, key)
			}
		}
	}
	c.accounts = next
	if epoch > 0 {
		c.epoch = epoch
	}
}

func (c *Chooser) Acquire(criteria contracts.Criteria) (contracts.Lease, error) {
	return c.AcquireExcluding(criteria, nil)
}

func (c *Chooser) AcquireExcluding(criteria contracts.Criteria, excluded map[string]struct{}) (contracts.Lease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	ids := make([]string, 0)
	quotaExcluded := false
	staleExcluded := false
	for id, account := range c.accounts {
		if _, skip := excluded[id]; skip {
			continue
		}
		if owner, ok := c.owner[id]; ok {
			if bucket := c.buckets[owner]; !bucket.validUntil.IsZero() && !bucket.validUntil.After(now) {
				staleExcluded = true
				continue
			}
		}
		if criteria.Platform != "" && account.Provider != criteria.Platform && account.Platform != criteria.Platform {
			continue
		}
		if account.Group != criteria.Group || account.Status != "active" {
			continue
		}
		if account.Credential.Kind != "static" && !account.Credential.ExpiresAt.IsZero() && !account.Credential.ExpiresAt.After(now) {
			continue
		}
		if !supports(account, criteria.Model) {
			continue
		}
		if blocked, ok := c.cooling[cooldownKey{id, criteria.Model}]; ok && blocked.until.After(now) {
			continue
		}
		if !hasQuota(account, criteria.Model) {
			quotaExcluded = true
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		if quotaExcluded {
			return contracts.Lease{}, ErrQuotaExhausted
		}
		if staleExcluded {
			return contracts.Lease{}, ErrSnapshotStale
		}
		return contracts.Lease{}, ErrNoAccount
	}
	sort.Strings(ids)
	if criteria.SessionKey != "" { /* sticky mapping is enforced by Redis in production; local order stays deterministic */
	}
	account := c.accounts[ids[0]]
	ttl := 0
	if !account.Credential.ExpiresAt.IsZero() {
		ttl = int(account.Credential.ExpiresAt.Sub(now).Seconds())
		if ttl < 0 {
			ttl = 0
		}
	}
	return contracts.Lease{AccountID: account.ID, Provider: account.Provider, Credential: account.Credential, Profile: account.Profile, Limits: account.Limits.WithDefaults(), TTL: ttl}, nil
}

func hasQuota(account contracts.Account, model string) bool {
	for _, item := range account.Quota.Items {
		if !quotaApplies(item, model) || quotaRemainingPositive(item) {
			continue
		}
		if item.RemainingExact != nil || item.RemainingFraction != nil || (item.Precision != nil && item.Precision.Remaining != nil) || item.Remaining != nil {
			return false
		}
	}
	return true
}

func quotaRemainingPositive(item contracts.QuotaItem) bool {
	if item.RemainingExact != nil {
		if sign, ok := item.RemainingExact.Sign(); ok {
			return sign > 0
		}
	}
	if item.RemainingFraction != nil {
		if sign, ok := item.RemainingFraction.Sign(); ok {
			return sign > 0
		}
	}
	if item.Precision != nil && item.Precision.Remaining != nil {
		if sign, ok := item.Precision.Remaining.Sign(); ok {
			return sign > 0
		}
	}
	return item.Remaining == nil || *item.Remaining > 0
}

func quotaApplies(item contracts.QuotaItem, model string) bool {
	switch item.Scope {
	case "account":
		return true
	case "model":
		return item.Model != "" && item.Model == model
	default:
		return false
	}
}

func supports(account contracts.Account, model string) bool {
	if model != "" {
		for _, excluded := range account.ExcludedModels {
			if excluded == model {
				return false
			}
		}
	}
	if model == "" || len(account.Capabilities) == 0 {
		return true
	}
	for _, capability := range account.Capabilities {
		if capability == model {
			return true
		}
	}
	return false
}

func (c *Chooser) MarkFailure(accountID, model string, class contracts.ErrorClass, reset time.Duration) {
	if class == contracts.ErrorForbiddenTransport || class == contracts.ErrorNetwork {
		return
	}
	key := cooldownKey{accountID: accountID, model: model}
	base := cooldownDuration(class)
	if class == contracts.ErrorRateLimitedUnknown {
		c.mu.Lock()
		c.failures[key]++
		count := c.failures[key]
		c.mu.Unlock()
		switch count {
		case 1:
			base = 5 * time.Second
		case 2:
			base = 15 * time.Second
		default:
			base = 60 * time.Second
		}
	}
	if reset > 0 {
		base = reset
	}
	base = withJitter(base)
	c.mu.Lock()
	defer c.mu.Unlock()
	until := c.now().Add(base)
	if class == contracts.ErrorAuthInvalid {
		until = c.now().Add(365 * 24 * time.Hour)
	}
	c.cooling[key] = cooldown{until: until, credentialVersion: c.accounts[accountID].Credential.Version}
}

func withJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return base
	}
	var raw [1]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return base
	}
	pct := time.Duration(int(raw[0]%41) - 20)
	return base * (100 + pct) / 100
}

func (c *Chooser) ClearAccount(accountID string, version int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, value := range c.cooling {
		if key.accountID == accountID && value.credentialVersion != version {
			delete(c.cooling, key)
		}
	}
}

func cooldownDuration(class contracts.ErrorClass) time.Duration {
	switch class {
	case contracts.ErrorAuthExpired:
		return 60 * time.Second
	case contracts.ErrorRateLimitedKnown:
		return 30 * time.Second
	case contracts.ErrorRateLimitedUnknown:
		return 15 * time.Second
	case contracts.ErrorUpstream5xx:
		return 3 * time.Second
	case contracts.ErrorBlocked:
		return 60 * time.Second
	default:
		return 0
	}
}
