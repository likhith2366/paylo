// Package risk implements the fraud rule engine (§14.2).
//
// Rules come before the ML model, deliberately. They deliver most of the value
// for the least effort, they are explainable — you can tell a customer exactly
// which rule declined them, which a gradient-boosted tree cannot — and they are
// the fallback the whole system leans on when the model service is degraded.
//
// The design constraint that shapes everything here: this runs synchronously
// inside the charge path with a sub-100ms budget. That rules out any rule
// needing a scan of transaction history. Velocity counters live in Redis,
// incremented per charge with a TTL, never recomputed by querying.
package risk

import (
	"fmt"
	"strings"
	"time"
)

// Level is the coarse decision the payment flow acts on.
type Level string

const (
	LevelLow    Level = "low"    // proceed
	LevelMedium Level = "medium" // step up to 3DS (§16) or queue for review
	LevelHigh   Level = "high"   // decline
)

// Action is what a single rule does when it fires.
type Action string

const (
	// ActionBlock declines outright — reserved for near-certain fraud.
	ActionBlock Action = "block"
	// ActionChallenge routes to 3DS rather than declining. The right default
	// for anything uncertain: a challenged legitimate customer completes their
	// purchase, a declined one leaves.
	ActionChallenge Action = "challenge"
	// ActionScore contributes to a cumulative score without deciding alone.
	ActionScore Action = "score"
	// ActionAllow short-circuits to approval, for explicit allowlists.
	ActionAllow Action = "allow"
)

// Assessment is the engine's verdict, including why.
//
// The reasons are not decoration: §14.5 requires storing which rules fired
// alongside the charge, so a decision can be explained months later during a
// dispute or an audit.
type Assessment struct {
	Level      Level    `json:"level"`
	Score      float64  `json:"score"`
	RulesFired []string `json:"rules_fired"`
	// Reason is the single most significant rule, suitable for showing a
	// merchant. Never shown to the cardholder — telling a fraudster which rule
	// caught them tells them what to change.
	Reason string `json:"reason,omitempty"`
}

// Transaction is everything the engine needs about one charge attempt.
type Transaction struct {
	MerchantID      string
	AmountCents     int64
	Currency        string
	CardFingerprint string
	CardBIN         string
	CardBrand       string
	Email           string
	IPAddress       string
	IPCountry       string
	BillingCountry  string
	CardCountry     string
	DeviceID        string
	Timestamp       time.Time

	// Precomputed counters, read from Redis rather than derived here.
	Velocity Velocity

	// Behavioral biometrics from the checkout iframe. Aggregates only — see
	// behavior.go for why nothing finer-grained crosses that boundary.
	Behavior Behavior

	// Merchant baselines, cached and refreshed periodically.
	MerchantAvgAmountCents int64
}

// Velocity holds the counters a rule can consult in constant time.
type Velocity struct {
	CardChargesLastHour   int
	CardChargesLastDay    int
	CardDeclinesLastHour  int
	IPChargesLastHour     int
	DeviceChargesLastHour int
	// Distinct cards seen on one device — the strongest single signal of
	// card testing, where a fraudster cycles stolen numbers through one browser.
	DeviceDistinctCards int
	EmailChargesLastDay int
}

// Rule is one named check.
type Rule struct {
	Name        string
	Description string
	Action      Action
	// Weight contributes to the cumulative score for ActionScore rules.
	Weight  float64
	Enabled bool
	// Evaluate reports whether the rule fires for this transaction.
	Evaluate func(Transaction, *Config) bool
}

// Config holds the tunable thresholds.
//
// Separated from the rule logic so thresholds can be adjusted without a code
// change — the seam a real risk team would tune through (§14.2). In production
// this loads from versioned YAML and hot-reloads.
type Config struct {
	MaxCardChargesPerHour   int     `yaml:"max_card_charges_per_hour"`
	MaxCardChargesPerDay    int     `yaml:"max_card_charges_per_day"`
	MaxCardDeclinesPerHour  int     `yaml:"max_card_declines_per_hour"`
	MaxIPChargesPerHour     int     `yaml:"max_ip_charges_per_hour"`
	MaxDeviceCardsPerDay    int     `yaml:"max_device_cards_per_day"`
	MaxEmailChargesPerDay   int     `yaml:"max_email_charges_per_day"`
	AmountAnomalyMultiplier float64 `yaml:"amount_anomaly_multiplier"`
	LargeAmountCents        int64   `yaml:"large_amount_cents"`

	HighRiskBINs      []string `yaml:"high_risk_bins"`
	HighRiskCountries []string `yaml:"high_risk_countries"`
	DisposableDomains []string `yaml:"disposable_email_domains"`

	// Score thresholds separating the three levels.
	MediumThreshold float64 `yaml:"medium_threshold"`
	HighThreshold   float64 `yaml:"high_threshold"`

	Behavior BehaviorConfig `yaml:"behavior"`
}

// DefaultConfig returns thresholds tuned to be noticeably conservative.
//
// The asymmetry is deliberate: a declined legitimate customer is a lost sale
// and often a lost relationship, while a missed fraudulent charge costs the
// disputed amount plus a fee. For most merchants the first is worse, so
// borderline cases challenge rather than block.
func DefaultConfig() *Config {
	return &Config{
		MaxCardChargesPerHour:   5,
		MaxCardChargesPerDay:    20,
		MaxCardDeclinesPerHour:  3,
		MaxIPChargesPerHour:     10,
		MaxDeviceCardsPerDay:    3,
		MaxEmailChargesPerDay:   15,
		AmountAnomalyMultiplier: 5.0,
		LargeAmountCents:        500_000, // $5,000
		HighRiskCountries:       []string{},
		DisposableDomains: []string{
			"mailinator.com", "guerrillamail.com", "10minutemail.com",
			"tempmail.com", "throwaway.email", "yopmail.com", "trashmail.com",
		},
		MediumThreshold: 30,
		HighThreshold:   70,
		Behavior:        DefaultBehaviorConfig(),
	}
}

// Engine evaluates rules in order.
type Engine struct {
	rules  []Rule
	config *Config
}

func NewEngine(config *Config) *Engine {
	if config == nil {
		config = DefaultConfig()
	}
	return &Engine{rules: append(defaultRules(), behaviorRules()...), config: config}
}

// Evaluate scores a transaction.
//
// Every rule runs even after one fires, rather than short-circuiting on the
// first hit. Two reasons: the cumulative score needs all contributions, and
// the stored explanation should list everything that was suspicious, not just
// whichever rule happened to be checked first.
func (e *Engine) Evaluate(txn Transaction) Assessment {
	var (
		score      float64
		fired      []string
		blocked    bool
		challenged bool
		reason     string
	)

	for _, rule := range e.rules {
		if !rule.Enabled || rule.Evaluate == nil {
			continue
		}
		if !rule.Evaluate(txn, e.config) {
			continue
		}

		fired = append(fired, rule.Name)
		switch rule.Action {
		case ActionAllow:
			// An explicit allowlist wins over everything.
			return Assessment{Level: LevelLow, Score: 0, RulesFired: []string{rule.Name}}
		case ActionBlock:
			blocked = true
			if reason == "" {
				reason = rule.Description
			}
		case ActionChallenge:
			challenged = true
			if reason == "" {
				reason = rule.Description
			}
		case ActionScore:
			score += rule.Weight
		}
	}

	level := LevelLow
	switch {
	case blocked || score >= e.config.HighThreshold:
		level = LevelHigh
	case challenged || score >= e.config.MediumThreshold:
		level = LevelMedium
	}

	if reason == "" && level != LevelLow && len(fired) > 0 {
		reason = fmt.Sprintf("%d risk signals present", len(fired))
	}

	return Assessment{Level: level, Score: score, RulesFired: fired, Reason: reason}
}

func defaultRules() []Rule {
	return []Rule{
		{
			Name:        "velocity_card_hourly",
			Description: "Unusually many charges on this card in the last hour",
			Action:      ActionChallenge,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				return t.Velocity.CardChargesLastHour > c.MaxCardChargesPerHour
			},
		},
		{
			Name:        "velocity_card_daily",
			Description: "Unusually many charges on this card today",
			Action:      ActionScore,
			Weight:      25,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				return t.Velocity.CardChargesLastDay > c.MaxCardChargesPerDay
			},
		},
		{
			// The clearest fraud signal in the set. A run of declines followed
			// by an attempt is someone working through stolen numbers until one
			// authorizes — a legitimate cardholder gives up long before this.
			Name:        "card_testing_declines",
			Description: "Repeated declines on this card suggest card testing",
			Action:      ActionBlock,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				return t.Velocity.CardDeclinesLastHour > c.MaxCardDeclinesPerHour
			},
		},
		{
			// One device, many different cards. A household shares a laptop
			// across two or three cards; twenty is a fraud ring (§14.4).
			Name:        "device_card_cycling",
			Description: "Many different cards used from one device",
			Action:      ActionBlock,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				return t.Velocity.DeviceDistinctCards > c.MaxDeviceCardsPerDay
			},
		},
		{
			Name:        "velocity_ip_hourly",
			Description: "Many charges from one IP address in the last hour",
			Action:      ActionScore,
			Weight:      20,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				return t.Velocity.IPChargesLastHour > c.MaxIPChargesPerHour
			},
		},
		{
			Name:        "amount_anomaly",
			Description: "Amount far exceeds this merchant's typical charge",
			Action:      ActionScore,
			Weight:      20,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				if t.MerchantAvgAmountCents <= 0 {
					// No baseline yet for a new merchant. Not firing is the
					// right call — inventing a baseline would flag their first
					// legitimate charges.
					return false
				}
				return float64(t.AmountCents) >
					float64(t.MerchantAvgAmountCents)*c.AmountAnomalyMultiplier
			},
		},
		{
			Name:        "large_amount",
			Description: "Unusually large charge",
			Action:      ActionScore,
			Weight:      15,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				return t.AmountCents > c.LargeAmountCents
			},
		},
		{
			// Fires only when all three signals disagree — nothing corroborates
			// anything.
			//
			// Counting DISTINCT countries, not pairwise mismatches. Pairwise is
			// the intuitive implementation and it is wrong: one odd value out of
			// three always produces two pairwise disagreements, so a US customer
			// travelling in Canada would trip a "two mismatches" threshold. Every
			// traveller would be flagged.
			//
			// Distinct counts behave the way the intent reads: all three agree
			// is 1, a traveller is 2, and only genuinely incoherent data is 3.
			Name:        "geo_mismatch",
			Description: "Billing country, card country, and IP location all disagree",
			Action:      ActionScore,
			Weight:      25,
			Enabled:     true,
			Evaluate: func(t Transaction, _ *Config) bool {
				seen := make(map[string]struct{}, 3)
				for _, country := range []string{t.IPCountry, t.BillingCountry, t.CardCountry} {
					if country != "" {
						seen[country] = struct{}{}
					}
				}
				// Requires all three to be present and mutually different.
				return len(seen) >= 3
			},
		},
		{
			Name:        "high_risk_bin",
			Description: "Card issued by a BIN on the risk list",
			Action:      ActionScore,
			Weight:      30,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				for _, bin := range c.HighRiskBINs {
					if t.CardBIN == bin {
						return true
					}
				}
				return false
			},
		},
		{
			Name:        "disposable_email",
			Description: "Disposable email address",
			Action:      ActionScore,
			Weight:      20,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				_, domain, found := strings.Cut(strings.ToLower(t.Email), "@")
				if !found {
					return false
				}
				for _, d := range c.DisposableDomains {
					if domain == d {
						return true
					}
				}
				return false
			},
		},
		{
			// Fraud clusters in the small hours of the cardholder's day, when
			// the real owner is asleep and won't notice an alert. Weak on its
			// own, useful in combination — hence a low weight.
			Name:        "odd_hour",
			Description: "Charge placed during low-activity overnight hours",
			Action:      ActionScore,
			Weight:      10,
			Enabled:     true,
			Evaluate: func(t Transaction, _ *Config) bool {
				h := t.Timestamp.UTC().Hour()
				return h >= 2 && h <= 5
			},
		},
	}
}

// Rules exposes the configured rules, for the dashboard and for tests.
func (e *Engine) Rules() []Rule { return e.rules }

// Config returns the active thresholds.
func (e *Engine) Config() *Config { return e.config }
