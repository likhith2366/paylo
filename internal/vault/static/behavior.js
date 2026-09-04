/**
 * Behavioral biometrics for the card input (§14.2 extension).
 *
 * How a card number is entered separates a cardholder from a fraudster
 * surprisingly well, and this iframe is the only place in the entire system
 * that can observe it — the merchant's page cannot reach inside, and by the
 * time the charge reaches the API the keystrokes are long gone.
 *
 * The signals, roughly in order of how much they are worth:
 *
 *   PASTE           A cardholder types the number off the card in their hand.
 *                   A fraudster pastes it from a list of stolen numbers. This
 *                   is the single most discriminating signal here and it costs
 *                   one event listener.
 *
 *   RHYTHM          Humans type unevenly — digits cluster in groups of four
 *                   with pauses between. Scripted input is metronomic. Low
 *                   inter-keystroke variance means automation.
 *
 *   CORRECTIONS     You rarely mistype your own card. You often mistype one
 *                   you are reading off another screen.
 *
 *   CVV HESITATION  A cardholder knows their CVV or reads it off the back in
 *                   one motion. Someone hunting for it in a dump pauses.
 *
 *   TOTAL DURATION  Both extremes are suspicious: implausibly fast means
 *                   automation, very slow means unfamiliarity.
 *
 * PRIVACY — the constraint that shapes this file.
 *
 * Keystroke dynamics are biometric data under GDPR Art. 9 and Illinois BIPA.
 * So nothing here records WHAT was typed, and nothing records a per-keystroke
 * timeline that could be replayed to reconstruct it. Only aggregates leave
 * this frame: counts, a mean, a variance, a coefficient of variation. Those
 * cannot be inverted into a card number, and they are not personal data on
 * their own.
 *
 * A per-keystroke timing array WOULD be more predictive. It is deliberately
 * not collected.
 */

export function createBehaviorTracker() {
  const state = {
    fields: {},
    startedAt: null,
    pasteCount: 0,
    pastedFields: [],
  };

  function field(name) {
    if (!state.fields[name]) {
      state.fields[name] = {
        keystrokes: 0,
        corrections: 0,
        intervals: [],
        focusedAt: null,
        firstKeyAt: null,
        lastKeyAt: null,
        pasted: false,
      };
    }
    return state.fields[name];
  }

  function attach(el, name) {
    const f = field(name);

    el.addEventListener('focus', () => {
      f.focusedAt = performance.now();
      if (state.startedAt === null) state.startedAt = f.focusedAt;
    });

    el.addEventListener('keydown', (e) => {
      const now = performance.now();

      // Backspace and Delete are corrections. Tracked as a rate rather than a
      // position, so nothing reveals which digit was wrong.
      if (e.key === 'Backspace' || e.key === 'Delete') {
        f.corrections++;
        return;
      }
      // Only count character input. Modifiers and navigation are not typing.
      if (e.key.length !== 1) return;

      f.keystrokes++;
      if (f.firstKeyAt === null) f.firstKeyAt = now;

      if (f.lastKeyAt !== null) {
        const gap = now - f.lastKeyAt;
        // Gaps over 5s are the user looking away, not typing rhythm. Including
        // them would swamp the variance with a single interruption.
        if (gap < 5000) f.intervals.push(gap);
      }
      f.lastKeyAt = now;
    });

    el.addEventListener('paste', () => {
      f.pasted = true;
      state.pasteCount++;
      if (!state.pastedFields.includes(name)) state.pastedFields.push(name);
    });
  }

  function stats(intervals) {
    if (intervals.length < 2) return { mean: null, cv: null };

    const mean = intervals.reduce((a, b) => a + b, 0) / intervals.length;
    const variance =
      intervals.reduce((sum, v) => sum + (v - mean) ** 2, 0) / intervals.length;
    const stddev = Math.sqrt(variance);

    // Coefficient of variation, not raw stddev: it is scale-free, so a fast
    // typist and a slow one with the same rhythm score alike. What separates
    // human from script is the EVENNESS, not the speed.
    return { mean: round(mean), cv: mean > 0 ? round(stddev / mean, 4) : null };
  }

  function round(n, places = 1) {
    const f = Math.pow(10, places);
    return Math.round(n * f) / f;
  }

  /**
   * Summarize into the aggregates that leave this frame.
   * Nothing here can be inverted into the entered values.
   */
  function summarize() {
    const number = field('number');
    const cvc = field('cvc');
    const numberStats = stats(number.intervals);

    // Time from first keystroke to last, across all fields.
    const ends = Object.values(state.fields)
      .map((f) => f.lastKeyAt)
      .filter((t) => t !== null);
    const totalMs =
      state.startedAt !== null && ends.length
        ? round(Math.max(...ends) - state.startedAt)
        : null;

    return {
      // The headline signal.
      pasted: state.pasteCount > 0,
      pasted_fields: state.pastedFields,

      // Rhythm. cv near 0 means metronomic, i.e. scripted.
      keystroke_interval_mean_ms: numberStats.mean,
      keystroke_interval_cv: numberStats.cv,

      // Effort. A high correction rate suggests reading an unfamiliar number.
      number_keystrokes: number.keystrokes,
      number_corrections: number.corrections,
      correction_rate:
        number.keystrokes > 0
          ? round(number.corrections / number.keystrokes, 3)
          : null,

      // Hesitation before the CVV: knowing it vs hunting for it.
      cvc_hesitation_ms:
        cvc.focusedAt !== null && cvc.firstKeyAt !== null
          ? round(cvc.firstKeyAt - cvc.focusedAt)
          : null,

      total_duration_ms: totalMs,
    };
  }

  return { attach, summarize };
}
