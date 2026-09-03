// Card network detection and validation (§2.3).
//
// A direct mirror of internal/payments/card.go. The two must agree: the browser
// uses this for instant feedback while typing, and the vault re-validates
// server-side because client-side checks are a UX feature, never a control.
//
// Entirely offline — no API key, no network call. This is the same logic
// Stripe Elements ships to browsers.

export const NETWORKS = {
  VISA: 'visa',
  MASTERCARD: 'mastercard',
  AMEX: 'amex',
  DISCOVER: 'discover',
  RUPAY: 'rupay',
  UNKNOWN: 'unknown',
};

// Ordering is load-bearing: RuPay overlaps Discover on 65 and 652x, so the
// more specific RuPay ranges are tested first. Keep in sync with card.go.
const RULES = [
  { network: NETWORKS.AMEX, digits: 2, ranges: [[34, 34], [37, 37]], lengths: [15] },
  { network: NETWORKS.RUPAY, digits: 3, ranges: [[508, 508], [606, 606], [652, 653], [817, 819]], lengths: [16] },
  { network: NETWORKS.VISA, digits: 1, ranges: [[4, 4]], lengths: [13, 16, 19] },
  { network: NETWORKS.MASTERCARD, digits: 2, ranges: [[51, 55]], lengths: [16] },
  { network: NETWORKS.MASTERCARD, digits: 4, ranges: [[2221, 2720]], lengths: [16] },
  { network: NETWORKS.DISCOVER, digits: 4, ranges: [[6011, 6011], [6221, 6229], [6440, 6499], [6500, 6599]], lengths: [16, 19] },
];

// Formatting groups per network. Amex is 4-6-5; everything else is 4-4-4-4.
const GROUPS = {
  [NETWORKS.AMEX]: [4, 6, 5],
  default: [4, 4, 4, 4],
};

export function onlyDigits(value) {
  return (value || '').replace(/\D/g, '');
}

// Works on partial input, which is what lets the network logo appear before the
// user has finished typing.
export function detectNetwork(value) {
  const digits = onlyDigits(value);
  if (!digits) return NETWORKS.UNKNOWN;

  for (const rule of RULES) {
    if (digits.length < rule.digits) continue;
    const prefix = parseInt(digits.slice(0, rule.digits), 10);
    for (const [lo, hi] of rule.ranges) {
      if (prefix >= lo && prefix <= hi) return rule.network;
    }
  }
  return NETWORKS.UNKNOWN;
}

// Mod-10 checksum. Catches typos and transpositions; says nothing about whether
// the card exists or has funds.
export function luhn(value) {
  const digits = onlyDigits(value);
  if (digits.length < 12) return false;

  let sum = 0;
  let double = false;
  for (let i = digits.length - 1; i >= 0; i--) {
    let d = digits.charCodeAt(i) - 48;
    if (double) {
      d *= 2;
      if (d > 9) d -= 9;
    }
    sum += d;
    double = !double;
  }
  return sum % 10 === 0;
}

export function lengthValid(network, length) {
  for (const rule of RULES) {
    if (rule.network === network && rule.lengths.includes(length)) return true;
  }
  return false;
}

// maxLength lets the input stop accepting digits at the right point per network.
export function maxLength(network) {
  const lengths = RULES.filter((r) => r.network === network).flatMap((r) => r.lengths);
  return lengths.length ? Math.max(...lengths) : 19;
}

export function formatNumber(value, network) {
  const digits = onlyDigits(value).slice(0, maxLength(network));
  const groups = GROUPS[network] || GROUPS.default;

  const parts = [];
  let offset = 0;
  for (const size of groups) {
    if (offset >= digits.length) break;
    parts.push(digits.slice(offset, offset + size));
    offset += size;
  }
  // Anything past the defined groups (19-digit Visa) trails on the end.
  if (offset < digits.length) parts.push(digits.slice(offset));

  return parts.join(' ');
}

// A card is valid through the last day of its stated expiry month.
export function expiryValid(month, year) {
  if (!(month >= 1 && month <= 12)) return false;

  const now = new Date();
  const currentYear = now.getFullYear();
  const fullYear = year < 100 ? 2000 + year : year;

  if (fullYear < currentYear) return false;
  if (fullYear === currentYear && month < now.getMonth() + 1) return false;
  // Reject implausibly distant dates, which are almost always typos.
  return fullYear <= currentYear + 20;
}

export function cvcLength(network) {
  return network === NETWORKS.AMEX ? 4 : 3;
}

export function validateCard({ number, expMonth, expYear, cvc }) {
  const errors = {};
  const network = detectNetwork(number);
  const digits = onlyDigits(number);

  if (!digits) {
    errors.number = 'Card number is required';
  } else if (!luhn(digits)) {
    errors.number = 'Card number is not valid';
  } else if (network !== NETWORKS.UNKNOWN && !lengthValid(network, digits.length)) {
    errors.number = 'Card number is the wrong length';
  }

  if (!expiryValid(expMonth, expYear)) {
    errors.expiry = 'Expiry date is not valid';
  }

  const wanted = cvcLength(network);
  if (onlyDigits(cvc).length !== wanted) {
    errors.cvc = `Security code must be ${wanted} digits`;
  }

  return { valid: Object.keys(errors).length === 0, errors, network };
}
