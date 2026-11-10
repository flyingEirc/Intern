package store

// PolicyTuning is the server-owned low-level tuning applied when a policy is
// created. It is not part of PolicyInput: business callers cannot choose these
// values, and a business edit never rewrites the historical columns.
type PolicyTuning struct {
	MaxChainLength int64
	Parallel       int64
	Throttle       int64
	ZstdLevel      int64
	PageTracking   bool
}

// DefaultPolicyTuning is the only source of new-policy tuning defaults.
//
// Seven chain entries keep a useful recovery window without unbounded growth;
// one worker is the conservative concurrency for a single control-plane host;
// zero leaves throttling to the surrounding runtime; three is the existing
// stored Zstandard level (the encoder currently runs at fixed concurrency and
// does not apply the level); page tracking stays off because the current
// physical chain does not depend on it.
func DefaultPolicyTuning() PolicyTuning {
	return PolicyTuning{
		MaxChainLength: 7,
		Parallel:       1,
		Throttle:       0,
		ZstdLevel:      3,
		PageTracking:   false,
	}
}
