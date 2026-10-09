package gate

import "testing"

func TestUnmeasuredRateByDim(t *testing.T) {
	// appropriate_band UNMEASURED in 2 of 3 runs; correct_diagnosis in 0 of 3.
	mk := func(abUnres bool) Verdict {
		return Verdict{Dims: []DimResult{
			{Dim: "appropriate_band", Unresolved: abUnres},
			{Dim: "correct_diagnosis", Unresolved: false},
		}}
	}
	rate := UnmeasuredRateByDim([]Verdict{mk(true), mk(true), mk(false)})
	if rate["appropriate_band"] != 0.67 {
		t.Errorf("appropriate_band UNMEASURED-rate = %v, want 0.67 (2 of 3)", rate["appropriate_band"])
	}
	if rate["correct_diagnosis"] != 0.00 {
		t.Errorf("correct_diagnosis UNMEASURED-rate = %v, want 0.00 (0 of 3)", rate["correct_diagnosis"])
	}
	// A dimension absent from some cards is not diluted: only its present runs count.
	partial := UnmeasuredRateByDim([]Verdict{
		{Dims: []DimResult{{Dim: "estate_grounded", Unresolved: true}}},
		{Dims: []DimResult{{Dim: "appropriate_band", Unresolved: false}}},
	})
	if partial["estate_grounded"] != 1.00 {
		t.Errorf("estate_grounded rate = %v, want 1.00 (present once, UNMEASURED that once)", partial["estate_grounded"])
	}
}

func TestUnmeasuredDimsReadsPerDim(t *testing.T) {
	v := Verdict{Dims: []DimResult{
		{Dim: "appropriate_band", Unresolved: true},
		{Dim: "correct_diagnosis", Unresolved: false},
		{Dim: "falsifiable_prediction", Unresolved: true},
	}}
	set := map[string]bool{}
	for _, d := range v.UnmeasuredDims() {
		set[d] = true
	}
	if !set["appropriate_band"] || !set["falsifiable_prediction"] || set["correct_diagnosis"] || len(set) != 2 {
		t.Errorf("UnmeasuredDims = %v, want exactly [appropriate_band falsifiable_prediction]", v.UnmeasuredDims())
	}
}
