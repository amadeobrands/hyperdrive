package state

import (
	"fmt"

	"github.com/renproject/hyperdrive/block"
)

type Machine interface {
	Height() block.Height
	Round() block.Round
	State() State
	Transition(transition Transition) Action
	Drop()
}

type machine struct {
	state  State
	height block.Height
	round  block.Round

	lockedRound *block.Round
	lockedBlock *block.SignedBlock

	polkaBuilder       block.PolkaBuilder
	commitBuilder      block.CommitBuilder
	consensusThreshold int
}

func NewMachine(state State, polkaBuilder block.PolkaBuilder, commitBuilder block.CommitBuilder, consensusThreshold int) Machine {
	return &machine{
		state:              state,
		polkaBuilder:       polkaBuilder,
		commitBuilder:      commitBuilder,
		consensusThreshold: consensusThreshold,
	}
}

func (machine *machine) Height() block.Height {
	return machine.height
}

func (machine *machine) Round() block.Round {
	return machine.round
}

func (machine *machine) State() State {
	return machine.state
}

func (machine *machine) Transition(transition Transition) Action {
	// Check pre-conditions
	if machine.lockedRound == nil {
		if machine.lockedBlock != nil {
			panic("expected locked block to be nil")
		}
	}
	if machine.lockedRound != nil {
		if machine.lockedBlock == nil {
			panic("expected locked round to be nil")
		}
	}

	switch machine.state.(type) {
	case WaitingForPropose:
		return machine.waitForPropose(transition)
	case WaitingForPolka:
		return machine.waitForPolka(transition)
	case WaitingForCommit:
		return machine.waitForCommit(transition)
	default:
		panic(fmt.Errorf("unexpected state type %T", machine.state))
	}
}

func (machine *machine) waitForPropose(transition Transition) Action {
	switch transition := transition.(type) {
	case Proposed:
		// FIXME: Proposals can (optionally) include a Polka to encourage
		// unlocking faster than would otherwise be possible.
		machine.state = WaitingForPolka{}
		return machine.preVote(transition.SignedBlock)

	case PreVoted:
		_ = machine.polkaBuilder.Insert(transition.SignedPreVote)

	case PreCommitted:
		_ = machine.commitBuilder.Insert(transition.SignedPreCommit)

	case TimedOut:
		machine.state = WaitingForPolka{}
		return machine.preVote(nil)

	default:
		panic(fmt.Errorf("unexpected transition type %T", transition))
	}

	return machine.checkCommonExitConditions()
}

func (machine *machine) waitForPolka(transition Transition) Action {
	switch transition := transition.(type) {
	case Proposed:
		// Ignore

	case PreVoted:
		if !machine.polkaBuilder.Insert(transition.SignedPreVote) {
			return nil
		}

		polka, _ := machine.polkaBuilder.Polka(machine.height, machine.consensusThreshold)
		if polka != nil && polka.Round == machine.round {
			machine.state = WaitingForCommit{}
			return machine.preCommit()
		}

	case PreCommitted:
		if !machine.commitBuilder.Insert(transition.SignedPreCommit) {
			return nil
		}

	case TimedOut:
		_, preVotingRound := machine.polkaBuilder.Polka(machine.height, machine.consensusThreshold)
		if preVotingRound == nil {
			return nil
		}

		machine.state = WaitingForCommit{}
		return machine.preCommit()

	default:
		panic(fmt.Errorf("unexpected transition type %T", transition))
	}

	return machine.checkCommonExitConditions()
}

func (machine *machine) waitForCommit(transition Transition) Action {
	switch transition := transition.(type) {
	case Proposed:
		// Ignore

	case PreVoted:
		_ = machine.polkaBuilder.Insert(transition.SignedPreVote)

	case PreCommitted:
		if !machine.commitBuilder.Insert(transition.SignedPreCommit) {
			return nil
		}

		commit, _ := machine.commitBuilder.Commit(machine.height, machine.consensusThreshold)
		if commit != nil && commit.Polka.Block == nil && commit.Polka.Round == machine.round {
			machine.state = WaitingForPropose{}
			machine.round++
			return Commit{
				Commit: block.Commit{
					Polka: block.Polka{
						Height: machine.height,
						Round:  machine.round,
					},
				},
			}
		}

	case TimedOut:
		_, preCommittingRound := machine.commitBuilder.Commit(machine.height, machine.consensusThreshold)
		if preCommittingRound == nil {
			return nil
		}

		machine.state = WaitingForPropose{}
		machine.round++
		return Commit{
			Commit: block.Commit{
				Polka: block.Polka{
					Height: machine.height,
					Round:  machine.round,
				},
			},
		}

	default:
		panic(fmt.Errorf("unexpected transition type %T", transition))
	}

	return machine.checkCommonExitConditions()
}

func (machine *machine) preVote(proposedBlock *block.SignedBlock) Action {
	polka, _ := machine.polkaBuilder.Polka(machine.height, machine.consensusThreshold)

	if machine.lockedRound != nil && polka != nil {
		// If the validator is locked on a block since LastLockRound but now has
		// a PoLC for something else at round PoLC-Round where LastLockRound <
		// PoLC-Round < R, then it unlocks.
		if *machine.lockedRound < polka.Round {
			machine.lockedRound = nil
			machine.lockedBlock = nil
		}
	}

	if machine.lockedRound != nil {
		// If the validator is still locked on a block, it prevotes that.
		return PreVote{
			PreVote: block.PreVote{
				Block:  machine.lockedBlock,
				Height: machine.height,
				Round:  machine.round,
			},
		}
	}

	if proposedBlock != nil && proposedBlock.Height == machine.height {
		// Else, if the proposed block from Propose(H,R) is good, it prevotes that.
		return PreVote{
			PreVote: block.PreVote{
				Block:  proposedBlock,
				Height: machine.height,
				Round:  machine.round,
			},
		}
	}

	// Else, if the proposal is invalid or wasn't received on time, it prevotes <nil>.
	return PreVote{
		PreVote: block.PreVote{
			Block:  nil,
			Height: machine.height,
			Round:  machine.round,
		},
	}
}

func (machine *machine) preCommit() Action {
	polka, _ := machine.polkaBuilder.Polka(machine.height, machine.consensusThreshold)

	if polka != nil {
		if polka.Block != nil {
			// If the validator has a PoLC at (H,R) for a particular block B, it
			// (re)locks (or changes lock to) and precommits B and sets LastLockRound =
			// R.
			machine.lockedRound = &polka.Round
			machine.lockedBlock = polka.Block
			return PreCommit{
				PreCommit: block.PreCommit{
					Polka: *polka,
				},
			}
		}

		// Else, if the validator has a PoLC at (H,R) for <nil>, it unlocks and
		// precommits <nil>.
		machine.lockedRound = nil
		machine.lockedBlock = nil
		return PreCommit{
			PreCommit: block.PreCommit{
				Polka: *polka,
			},
		}
	}

	// Else, it keeps the lock unchanged and precommits <nil>.
	return PreCommit{
		PreCommit: block.PreCommit{
			Polka: block.Polka{
				Height: machine.height,
				Round:  machine.round,
			},
		},
	}
}

func (machine *machine) checkCommonExitConditions() Action {
	// Get the Commit for the current Height and the latest Round
	commit, preCommittingRound := machine.commitBuilder.Commit(machine.height, machine.consensusThreshold)
	if commit != nil && commit.Polka.Block != nil {
		// After +2/3 precommits for a particular block. --> goto Commit(H)
		machine.state = WaitingForPropose{}
		machine.height = commit.Polka.Height + 1
		machine.round = 0
		machine.lockedBlock = nil
		machine.lockedRound = nil
		return Commit{Commit: *commit}
	}

	// Get the Polka for the current Height and the latest Round
	_, preVotingRound := machine.polkaBuilder.Polka(machine.height, machine.consensusThreshold)
	if preVotingRound != nil && *preVotingRound > machine.round {
		// After any +2/3 prevotes received at (H,R+x). --> goto Prevote(H,R+x)
		machine.round = *preVotingRound
		return machine.preVote(nil)
	}

	if preCommittingRound != nil && *preCommittingRound > machine.round {
		// After any +2/3 precommits received at (H,R+x). --> goto Precommit(H,R+x)
		machine.state = WaitingForCommit{}
		machine.round = *preCommittingRound
		return machine.preCommit()
	}

	return nil
}

func (machine *machine) Drop() {
	machine.polkaBuilder.Drop(machine.height)
	machine.commitBuilder.Drop(machine.height)
}
