package scheduler

import "wsms/internal/wsl"

// L1Selection is the material selected for the active capsule.
type L1Selection struct {
	Task         *wsl.TaskRecord
	Hard         []*wsl.ConstraintRecord
	Soft         []*wsl.ConstraintRecord
	LastFailure  *wsl.FailureRecord
	Avoids       []*wsl.AvoidRecord
	Next         *wsl.NextRecord
}

// SelectL1 picks resident working-state for rendering.
func SelectL1(st *wsl.WorkingState) L1Selection {
	return L1Selection{
		Task:        st.ActiveTask(),
		Hard:        st.HardConstraints(),
		Soft:        st.SoftConstraints(),
		LastFailure: st.LastFailure(),
		Avoids:      st.Avoids(),
		Next:        st.Next(),
	}
}
