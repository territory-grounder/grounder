package skillstore

import "math"

// The one-sided Welch t-test (H0: mean(a) <= mean(b)) — a transliteration of the predecessor's
// prompt_patch_trial.py welch_one_sided, which was proven under fire: its earlier normal approximation
// was anti-conservative at K=15–30 samples (IFRNLLEI01PRD-1096), so this uses the proper Student-t
// tail with Welch–Satterthwaite degrees of freedom via the stdlib-only regularized incomplete beta
// (Numerical Recipes continued fraction). Golden-value tests pin this implementation to the Python
// outputs digit-for-digit (spec/014 REQ-1308) — including its edge semantics: too-few samples →
// (0, 1) never-significant; zero pooled variance → (0, 0.5) undecidable.

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// sampleVar is the predecessor's _var: the n-1 sample variance.
func sampleVar(xs []float64) float64 {
	m := mean(xs)
	s := 0.0
	for _, x := range xs {
		d := x - m
		s += d * d
	}
	return s / float64(len(xs)-1)
}

// betacf is the continued fraction for the incomplete beta (Numerical Recipes), stdlib only.
func betacf(a, b, x float64) float64 {
	const (
		maxIt = 200
		eps   = 3.0e-12
		fpMin = 1.0e-300
	)
	qab, qap, qam := a+b, a+1.0, a-1.0
	c := 1.0
	d := 1.0 - qab*x/qap
	if math.Abs(d) < fpMin {
		d = fpMin
	}
	d = 1.0 / d
	h := d
	for m := 1; m <= maxIt; m++ {
		m2 := float64(2 * m)
		mf := float64(m)
		aa := mf * (b - mf) * x / ((qam + m2) * (a + m2))
		d = 1.0 + aa*d
		if math.Abs(d) < fpMin {
			d = fpMin
		}
		c = 1.0 + aa/c
		if math.Abs(c) < fpMin {
			c = fpMin
		}
		d = 1.0 / d
		h *= d * c
		aa = -(a + mf) * (qab + mf) * x / ((a + m2) * (qap + m2))
		d = 1.0 + aa*d
		if math.Abs(d) < fpMin {
			d = fpMin
		}
		c = 1.0 + aa/c
		if math.Abs(c) < fpMin {
			c = fpMin
		}
		d = 1.0 / d
		delta := d * c
		h *= delta
		if math.Abs(delta-1.0) < eps {
			break
		}
	}
	return h
}

// betai is the regularized incomplete beta I_x(a, b), stdlib only.
func betai(a, b, x float64) float64 {
	if x <= 0.0 {
		return 0.0
	}
	if x >= 1.0 {
		return 1.0
	}
	lg1, _ := math.Lgamma(a + b)
	lg2, _ := math.Lgamma(a)
	lg3, _ := math.Lgamma(b)
	bt := math.Exp(lg1 - lg2 - lg3 + a*math.Log(x) + b*math.Log(1.0-x))
	if x < (a+1.0)/(a+b+2.0) {
		return bt * betacf(a, b, x) / a
	}
	return 1.0 - bt*betacf(b, a, 1.0-x)/b
}

// studentTSF is the one-sided upper-tail survival P(T > t) for a Student-t with df degrees.
func studentTSF(t, df float64) float64 {
	if df <= 0 {
		return 0.5
	}
	x := df / (df + t*t)
	tail := 0.5 * betai(df/2.0, 0.5, x) // = P(T >= |t|)
	if t > 0 {
		return tail
	}
	return 1.0 - tail
}

// WelchOneSided returns (t, p) for H0: mean(a) <= mean(b) — the trial finalizer's sole significance
// test (candidate arm a vs the CONCURRENT control arm b, REQ-1308).
func WelchOneSided(a, b []float64) (float64, float64) {
	na, nb := len(a), len(b)
	if na < 2 || nb < 2 {
		return 0.0, 1.0
	}
	va, vb := sampleVar(a), sampleVar(b)
	se := math.Sqrt(va/float64(na) + vb/float64(nb))
	if se == 0 {
		return 0.0, 0.5
	}
	t := (mean(a) - mean(b)) / se
	num := math.Pow(va/float64(na)+vb/float64(nb), 2)
	den := math.Pow(va/float64(na), 2)/float64(na-1) + math.Pow(vb/float64(nb), 2)/float64(nb-1)
	df := float64(na + nb - 2)
	if den > 0 {
		df = num / den
	}
	return t, studentTSF(t, df)
}
