package risk

// Behavioral biometrics rules (§14.2 extension).
//
// These are RULES, not model features, and that is not a shortcut. The training
// data has no keystroke information at all, so there is no weight to learn —
// any number the model produced for these would be invented. Encoding them as
// explicit, explainable rules is the honest option, and it is exactly what the
// rule engine exists for: policy we can state and defend, sitting underneath a
// model that cannot override it.
//
// When real traffic accumulates, chargeback outcomes become labels for these
// signals and they can graduate into the model (§14.3). Until then they stay
// here.
//
// A deliberate calibration note: nearly all of these CHALLENGE rather than
// BLOCK. Pasting a card number is common and legitimate — password managers do
// it, and so does anyone who keeps their card in a notes app. Blocking on paste
// alone would decline a large number of real customers to catch a few
// fraudsters, which is the wrong trade for almost every merchant. The one
// exception is a paste combined with machine-like rhythm, which no human
// produces.

// Behavior carries the aggregates collected by the checkout iframe.
//
// Note what is absent: any per-keystroke timing array, and anything that
// records what was typed. Keystroke dynamics are biometric data under GDPR
// Art. 9 and Illinois BIPA, so only non-invertible aggregates cross the
// boundary. A full timing sequence would predict better and is deliberately
// not collected.
type Behavior struct {
	// Present reports whether the iframe supplied any data. Old widget
	// versions and non-browser API callers will not, and their absence must
	// not be read as suspicious — see the guard on every rule below.
	Present bool

	Pasted       bool
	PastedFields []string

	// KeystrokeIntervalCV is the coefficient of variation of inter-keystroke
	// gaps. Scale-free on purpose: a fast typist and a slow one with the same
	// rhythm score alike. Human typing is uneven (CV typically 0.4-1.2);
	// scripted input is metronomic (CV near 0).
	KeystrokeIntervalCV   *float64
	KeystrokeIntervalMean *float64

	NumberKeystrokes int
	CorrectionRate   *float64
	CVCHesitationMs  *float64
	TotalDurationMs  *float64
}

// Thresholds, added to Config.
type BehaviorConfig struct {
	// Below this CV, typing is too even to be human.
	MinKeystrokeCV float64 `yaml:"min_keystroke_cv"`
	// Above this correction rate, the typist is likely reading an unfamiliar
	// number rather than one they own.
	MaxCorrectionRate float64 `yaml:"max_correction_rate"`
	// A long pause before the CVV suggests hunting for it.
	MaxCVCHesitationMs float64 `yaml:"max_cvc_hesitation_ms"`
	// Faster than a person can physically type a 16-digit number.
	MinTotalDurationMs float64 `yaml:"min_total_duration_ms"`
}

func DefaultBehaviorConfig() BehaviorConfig {
	return BehaviorConfig{
		// Human inter-keystroke CV is rarely under ~0.25 even for practised
		// typists; digits cluster in groups with pauses between.
		MinKeystrokeCV: 0.15,
		// Mistyping a fifth of the digits of a card you are holding is
		// implausible.
		MaxCorrectionRate: 0.20,
		// Eight seconds staring at a CVV field you have already focused.
		MaxCVCHesitationMs: 8000,
		// ~1.2s is about the floor for a human entering 16 digits plus expiry
		// and CVV. Below that is automation, not a fast typist.
		MinTotalDurationMs: 1200,
	}
}

func behaviorRules() []Rule {
	return []Rule{
		{
			// Machine-paced typing. The clearest automation signal in the set:
			// humans do not type at a constant interval, and the CV is
			// scale-free so this does not simply flag fast typists.
			Name:        "automation_typing_rhythm",
			Description: "Keystroke timing is too regular to be human",
			Action:      ActionChallenge,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				b := t.Behavior
				// Needs enough keystrokes for the statistic to mean anything.
				if !b.Present || b.KeystrokeIntervalCV == nil || b.NumberKeystrokes < 8 {
					return false
				}
				return *b.KeystrokeIntervalCV < c.Behavior.MinKeystrokeCV
			},
		},
		{
			// Paste AND machine rhythm together. Either alone is weak; both at
			// once is a script filling a form, which no legitimate customer
			// produces.
			Name:        "automated_card_entry",
			Description: "Card details were pasted and entered at machine speed",
			Action:      ActionBlock,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				b := t.Behavior
				if !b.Present || !b.Pasted {
					return false
				}
				tooFast := b.TotalDurationMs != nil && *b.TotalDurationMs < c.Behavior.MinTotalDurationMs
				tooEven := b.KeystrokeIntervalCV != nil && b.NumberKeystrokes >= 8 &&
					*b.KeystrokeIntervalCV < c.Behavior.MinKeystrokeCV
				return tooFast || tooEven
			},
		},
		{
			// Paste on its own. Weighted low deliberately: password managers
			// paste, and plenty of people keep a card in a notes app. It is a
			// signal, not a verdict.
			Name:        "card_number_pasted",
			Description: "Card number was pasted rather than typed",
			Action:      ActionScore,
			Weight:      15,
			Enabled:     true,
			Evaluate: func(t Transaction, _ *Config) bool {
				return t.Behavior.Present && t.Behavior.Pasted
			},
		},
		{
			// You rarely mistype the card in your hand. You often mistype one
			// you are reading off another screen.
			Name:        "high_correction_rate",
			Description: "Card number was corrected unusually often while typing",
			Action:      ActionScore,
			Weight:      15,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				b := t.Behavior
				if !b.Present || b.CorrectionRate == nil || b.NumberKeystrokes < 8 {
					return false
				}
				return *b.CorrectionRate > c.Behavior.MaxCorrectionRate
			},
		},
		{
			// A cardholder knows their CVV or turns the card over. Hunting for
			// it in a list takes noticeably longer.
			Name:        "cvc_hesitation",
			Description: "Long pause before entering the security code",
			Action:      ActionScore,
			Weight:      10,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				b := t.Behavior
				if !b.Present || b.CVCHesitationMs == nil {
					return false
				}
				return *b.CVCHesitationMs > c.Behavior.MaxCVCHesitationMs
			},
		},
		{
			// Faster than a person can physically enter the fields, with no
			// paste to explain it — a script driving the DOM directly.
			Name:        "impossible_entry_speed",
			Description: "Card details were entered faster than a person can type",
			Action:      ActionChallenge,
			Enabled:     true,
			Evaluate: func(t Transaction, c *Config) bool {
				b := t.Behavior
				if !b.Present || b.TotalDurationMs == nil || b.Pasted {
					return false
				}
				// Requires real keystrokes: a legitimately autofilled form has
				// a short duration and no keystrokes, and must not be flagged.
				return b.NumberKeystrokes >= 8 && *b.TotalDurationMs < c.Behavior.MinTotalDurationMs
			},
		},
	}
}
