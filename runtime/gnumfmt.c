/*
** gnumfmt.c — fork-exact number→string conversion (golua55 dialect).
**
** golua55 renders a number through LNumber.String():
**
**   if float64(v) == float64(int64(v))  → fmt.Sprint(int64(v))   // integer digits
**   else                                → fmt.Sprint(float64(v)) // Go %g, shortest
**
** Stock C Lua 5.1 instead sprintf's "%.14g" (LUA_NUMBER_FMT). The two agree
** on small integers but diverge everywhere else: integer-valued floats past
** 14 significant digits (1e15+100 → fork "1000000000000100" vs C
** "1.0000000000001e+15"), shortest-round-trip fractions (3.5/3 →
** "1.1666666666666667" vs "1.1666666666667"), ±Inf/NaN texts ("+Inf"/"NaN"
** vs "inf"/"nan"), and the -0.0 sign (fork int64(-0.0) → "0" vs C "-0").
**
** The interpreter is the spec (CLAUDE.md), so this file ports the
** interpreter's exact algorithm: Go's strconv shortest 'g' formatting as of
** Go 1.27 (internal/strconv uscale.go/ftoa.go — "Floating-Point Printing
** and Parsing Can Be Simple And Fast", https://research.swtch.com/fp;
** BSD-licensed), plus the fork's integer fast path.
**
** Two platform caveats are pinned deliberately (the interp oracle runs on
** darwin/arm64; the blob is deterministic wasm wherever it runs):
**   - float64→int64 conversion out of range SATURATES (arm64 FCVTZS
**     semantics): 2^63 converts to MaxInt64 — and float64(MaxInt64) == 2^63,
**     so tostring(2^63) is "9223372036854775807"; -2^63 likewise to MinInt64.
**   - anything above 2^63 (or below -2^63, other than -2^63 exactly) fails
**     the integer test and takes the %g path.
**
** gn_lua_number_to_string is pure (no rt_* state); it is shared by
** lua_number2str (lvm.c — tostring/concat/table.concat) and the gopher
** error-text key repr (rt_grepr). It needs ≤ 25 bytes + NUL.
*/

#include <stdint.h>
#include <string.h>

#include "gnumfmt.h"

/* ---- pow10 table: 128-bit mantissas of 10**p, high bit set, for
** p = GN_POW10_MIN (-348) .. GN_POW10_MAX (347). Generated from Go's
** internal/strconv/pow10tab.go ("go run pow10gen.go. DO NOT EDIT."). */
#define GN_POW10_MIN (-348)
#define GN_POW10_MAX 347

static const struct { uint64_t hi, lo; } gn_pow10[] = {
#include "gnumfmt_pow10.h"
};

/* ---- small helpers (ports of Go's bits/math inline code) ---- */

static int gn_len64(uint64_t x) { /* bits.Len64 */
  return 64 - (x ? __builtin_clzll(x) : 64);
}

static void gn_mul64(uint64_t a, uint64_t b, uint64_t *hi, uint64_t *lo) {
  __uint128_t r = (__uint128_t)a * b;
  *hi = (uint64_t)(r >> 64);
  *lo = (uint64_t)r;
}

/* log10Pow2(x) = ⌊x·log₁₀2⌋ */
static int gn_log10pow2(int x) { return (int)(((int64_t)x * 78913) >> 18); }
/* log2Pow10(x) = ⌊x·log₂10⌋ */
static int gn_log2pow10(int x) { return (int)(((int64_t)x * 108853) >> 15); }
/* skewed(e) = ⌊log₁₀ 3/4·2^e⌋ — decimal footprint of a power-of-two's
** asymmetric rounding interval */
static int gn_skewed(int e) { return (int)(((int64_t)e * 631305 - 261663) >> 21); }

static const uint64_t gn_u64pow10[20] = {
    1,                   10ULL,                100ULL,                1000ULL,
    10000ULL,            100000ULL,            1000000ULL,            10000000ULL,
    100000000ULL,        1000000000ULL,        10000000000ULL,        100000000000ULL,
    1000000000000ULL,    10000000000000ULL,    100000000000000ULL,    1000000000000000ULL,
    10000000000000000ULL, 100000000000000000ULL, 1000000000000000000ULL, 10000000000000000000ULL,
};

/* numDigits(d) = decimal digit count, d ≥ 1 */
static int gn_num_digits(uint64_t d) {
  int nd = gn_log10pow2(gn_len64(d));
  return nd + (d >= gn_u64pow10[nd]);
}

/* formatBase10: the nd decimal digits of u, most-significant first */
static void gn_format_base10(char *a, int nd, uint64_t u) {
  int i;
  for (i = nd - 1; i >= 0; i--) {
    a[i] = (char)('0' + (int)(u % 10));
    u /= 10;
  }
}

/* ---- unrounded: a value carried in units of 1/4 with sticky low bits
** (port of Go's `unrounded`); the low 2 bits track exactness so that
** floor/round/ceil never lose "is there anything more below" */

typedef uint64_t gn_un;

static uint64_t gn_floor(gn_un u) { return u >> 2; }
static uint64_t gn_round(gn_un u) { return (u + 1 + ((u >> 2) & 1)) >> 2; }
static uint64_t gn_ceil(gn_un u) { return (u + 3) >> 2; }
static gn_un gn_nudge(gn_un u, int delta) { return u + (gn_un)(int64_t)delta; }

/* ---- the unrounded scaler (Go's prescale/uscale) ---- */

typedef struct {
  uint64_t pm_hi, pm_lo;
  int s;
} gn_scaler;

static void gn_prescale(gn_scaler *pre, int e, int p) {
  pre->pm_hi = gn_pow10[p - GN_POW10_MIN].hi;
  pre->pm_lo = gn_pow10[p - GN_POW10_MIN].lo;
  pre->s = -(e + gn_log2pow10(p) + 3);
}

/* uscale: unround(x · 2^e · 10^p); x must be left-justified (high bit set)
** and pre must come from gn_prescale(e, p) */
static gn_un gn_uscale(uint64_t x, const gn_scaler *pre) {
  uint64_t hi, mid, mid2, lo;
  int s = pre->s & 63;
  gn_mul64(x, pre->pm_hi, &hi, &mid);
  if ((hi >> s << s) != hi)
    return (gn_un)((hi >> s) | 1);
  gn_mul64(x, pre->pm_lo, &mid2, &lo);
  (void)lo;
  hi -= (mid < mid2);
  return (gn_un)((hi >> s) | (mid - mid2 > 1));
}

/* ---- shortFloat: the shortest decimal d·10^p that identifies f = m·2^e
** (m left-justified 64-bit). Direct port of Go's uscale.go shortFloat. */

static void gn_short_float(uint64_t m, int e, uint64_t *dout, int *pout) {
  const int mant_bits = 52; /* float64MantBits */
  const int min_exp = -1085; /* float64MinExp */
  uint64_t mn, mx;
  int odd, p, z = 63 - mant_bits;
  gn_scaler pre;
  uint64_t dmin, dmax, d;

  if (m == ((uint64_t)1 << 63) && e > min_exp) {
    /* power of two (except the lowest normal): interval is skewed —
    ** lower boundary at 1/4 ulp, upper at 1/2 ulp */
    p = -gn_skewed(e + z);
    mn = m - ((uint64_t)1 << (z - 2));
    mx = m + ((uint64_t)1 << (z - 1));
    odd = (int)((m >> z) & 1);
  } else if (e >= min_exp) {
    p = -gn_log10pow2(e + z);
    mn = m - ((uint64_t)1 << (z - 1));
    mx = m + ((uint64_t)1 << (z - 1));
    odd = (int)((m >> z) & 1);
  } else {
    /* subnormal/lowest-normal: the interval narrows by the missing
    ** precision — widen z to the actual spacing */
    z = z + (min_exp - e);
    p = -gn_log10pow2(e + z);
    mn = m - ((uint64_t)1 << (z - 1));
    mx = m + ((uint64_t)1 << (z - 1));
    odd = (int)((m >> z) & 1);
  }

  gn_prescale(&pre, e, p);
  dmin = gn_ceil(gn_nudge(gn_uscale(mn, &pre), odd));
  dmax = gn_floor(gn_nudge(gn_uscale(mx, &pre), -odd));

  /* NB: Go's `d` is the named uint64 return — plain division, NO
  ** unrounded sticky div (that is the fixed-precision path's div). */
  d = dmax / 10;
  if (d * 10 >= dmin) { /* one digit shorter still identifies the interval */
    *dout = d;
    *pout = -(p - 1);
    return;
  }
  d = dmin;
  if (d < dmax) /* no single leading candidate — round m itself */
    d = gn_round(gn_uscale(m, &pre));
  *dout = d;
  *pout = -p;
}

/* ---- fmtEFG 'g'/'e'/'f' rendering (shortest mode only) ----
**
** digs[0..nd) are the digits, value = 0.digs × 10^dp. In shortest mode Go
** picks 'e' when the decimal exponent (dp-1) is < -4 or ≥ 6 — the "%v turns
** scientific at 1e±N" cutoff — with e-precision nd-1 and f-precision
** max(nd-dp, 0). */

static char *gn_fmt_efg(char *dst, int neg, const char *digs, int dp, int nd) {
  char *o = dst;
  int prec, exp;

  if (neg)
    *o++ = '-';

  exp = dp - 1;
  if (exp < -4 || exp >= 6) { /* 'e': -d.ddddde±dd */
    int i, m;
    *o++ = nd ? digs[0] : '0';
    prec = nd - 1;
    if (prec > 0) {
      *o++ = '.';
      i = 1;
      m = nd < prec + 1 ? nd : prec + 1;
      for (; i < m; i++)
        *o++ = digs[i];
      for (; i < prec + 1; i++)
        *o++ = '0';
    }
    *o++ = 'e';
    if (nd == 0)
      exp = 0;
    if (exp < 0) {
      *o++ = '-';
      exp = -exp;
    } else {
      *o++ = '+';
    }
    if (exp < 10) {
      *o++ = '0';
      *o++ = (char)('0' + exp);
    } else if (exp < 100) {
      *o++ = (char)('0' + exp / 10);
      *o++ = (char)('0' + exp % 10);
    } else {
      *o++ = (char)('0' + exp / 100);
      *o++ = (char)('0' + (exp / 10) % 10);
      *o++ = (char)('0' + exp % 10);
    }
  } else { /* 'f': -dddd.ddd */
    int m, lz, off, tz;
    prec = nd - dp; /* max(prec-dp, 0) with prec=nd */
    if (prec < 0)
      prec = 0;
    if (dp > 0) {
      m = nd < dp ? nd : dp;
      memcpy(o, digs, (size_t)m);
      o += m;
      for (m = dp - m; m > 0; m--)
        *o++ = '0';
    } else {
      *o++ = '0';
    }
    if (prec > 0) {
      *o++ = '.';
      lz = prec < (dp < 0 ? -dp : 0) ? prec : (dp < 0 ? -dp : 0);
      off = dp + lz;
      m = (prec - lz) < (nd - off > 0 ? nd - off : 0) ? (prec - lz) : (nd - off > 0 ? nd - off : 0);
      if (m < 0)
        m = 0;
      tz = prec - lz - m;
      while (lz-- > 0)
        *o++ = '0';
      memcpy(o, digs + off, (size_t)m);
      o += m;
      while (tz-- > 0)
        *o++ = '0';
    }
  }
  *o = '\0';
  return o;
}

/* gn_fmt_g_shortest: strconv.FormatFloat(v, 'g', -1, 64) for finite v.
** Returns bytes written (excl. NUL). NaN/Inf handled by the caller. */
int gn_fmt_g_shortest(char *dst, double v) {
  uint64_t b, mant;
  int neg, exp_field, exp, s;
  uint64_t m, d;
  int e, p, nd, dp;
  char digs[32];

  memcpy(&b, &v, 8);
  neg = (int)(b >> 63);
  exp_field = (int)((b >> 52) & 0x7FF);
  mant = b & (((uint64_t)1 << 52) - 1);
  /* callers route NaN/Inf away, but keep this self-contained */
  if (exp_field == 0x7FF) {
    if (mant)
      return (int)strlen(strcpy(dst, "NaN"));
    return (int)strlen(strcpy(dst, neg ? "-Inf" : "+Inf"));
  }
  if (exp_field == 0)
    exp = 1 - 1023; /* subnormal: implicit exponent of the lowest normal */
  else {
    mant |= (uint64_t)1 << 52;
    exp = exp_field - 1023; /* float64Bias; value = mant·2^(exp-52) */
  }

  if (mant == 0) { /* ±0: f-form with no digits */
    digs[0] = '0'; /* not read: nd == 0 */
    return (int)(gn_fmt_efg(dst, neg, digs, 0, 0) - dst);
  }

  s = 64 - gn_len64(mant);
  m = mant << s;
  e = exp - s;
  gn_short_float(m, e - 52, &d, &p);

  nd = gn_num_digits(d);
  gn_format_base10(digs, nd, d);
  dp = nd + p;
  while (nd > 0 && digs[nd - 1] == '0')
    nd--; /* setDigits trims trailing zeros */

  return (int)(gn_fmt_efg(dst, neg, digs, dp, nd) - dst);
}

/* ---- the fork's LNumber.String() ----
**
** float64→int64 saturating semantics (arm64, the oracle platform):
** ≥ 2^63 converts to MaxInt64, ≤ -2^63 to MinInt64; only exact ±2^63
** survives the round trip float64(int64(v)) == v. */

static int gn_is_integer(double v, int64_t *out) {
  if (v >= 9223372036854775808.0) { /* ≥ 2^63 */
    if (v == 9223372036854775808.0) {
      *out = INT64_MAX;
      return 1;
    }
    return 0;
  }
  if (v < -9223372036854775808.0) /* < -2^63 (never integer by the fork's
                                      round trip: int64(v) saturates to
                                      MinInt64 = -2^63 ≠ v) */
    return 0;
  if (v == -9223372036854775808.0) {
    *out = INT64_MIN;
    return 1;
  }
  {
    int64_t i = (int64_t)v; /* |v| < 2^63: in range, truncates like Go */
    if ((double)i != v)
      return 0;
    *out = i;
  }
  return 1;
}

void gn_lua_number_to_string(char *buf, double v) {
  uint64_t b;
  int64_t i;

  memcpy(&b, &v, 8);
  if (((b >> 52) & 0x7FF) == 0x7FF) { /* NaN/±Inf: fmt.Sprint texts */
    if (b & (((uint64_t)1 << 52) - 1))
      strcpy(buf, "NaN");
    else
      strcpy(buf, (b >> 63) ? "-Inf" : "+Inf");
    return;
  }
  if (gn_is_integer(v, &i)) {
    /* fmt.Sprint(int64) — plain %lld */
    char tmp[24];
    char *o = tmp + sizeof tmp;
    uint64_t u;
    *--o = '\0';
    if (i < 0) {
      u = (uint64_t)(-(i + 1)) + 1; /* INT64_MIN safe */
      *buf = '-';
      buf++;
    } else {
      u = (uint64_t)i;
    }
    do {
      *--o = (char)('0' + (int)(u % 10));
      u /= 10;
    } while (u);
    strcpy(buf, o);
    return;
  }
  gn_fmt_g_shortest(buf, v);
}
