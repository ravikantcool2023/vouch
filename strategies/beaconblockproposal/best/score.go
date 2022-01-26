// Copyright © 2020 - 2022 Attestant Limited.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package best

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/altair"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/prysmaticlabs/go-bitfield"
)

// scoreBeaconBlockPropsal generates a score for a beacon block.
// The score is relative to the reward expected by proposing the block.
func (s *Service) scoreBeaconBlockProposal(ctx context.Context,
	name string,
	parentSlot phase0.Slot,
	blockProposal *spec.VersionedBeaconBlock,
) float64 {
	if blockProposal == nil {
		return 0
	}
	if blockProposal.IsEmpty() {
		return 0
	}

	switch blockProposal.Version {
	case spec.DataVersionPhase0:
		return s.scorePhase0BeaconBlockProposal(ctx, name, parentSlot, blockProposal.Phase0)
	case spec.DataVersionAltair:
		return s.scoreAltairBeaconBlockProposal(ctx, name, parentSlot, blockProposal.Altair)
	default:
		log.Error().Int("version", int(blockProposal.Version)).Msg("Unhandled block version")
		return 0
	}
}

// scorePhase0BeaconBlockPropsal generates a score for a phase 0 beacon block.
func (*Service) scorePhase0BeaconBlockProposal(_ context.Context,
	name string,
	parentSlot phase0.Slot,
	blockProposal *phase0.BeaconBlock,
) float64 {
	immediateAttestationScore := float64(0)
	attestationScore := float64(0)

	// We need to avoid duplicates in attestations.
	// Map is attestation slot -> committee index -> validator committee index -> aggregate.
	attested := make(map[phase0.Slot]map[phase0.CommitteeIndex]bitfield.Bitlist)
	for _, attestation := range blockProposal.Body.Attestations {
		data := attestation.Data
		if _, exists := attested[data.Slot]; !exists {
			attested[data.Slot] = make(map[phase0.CommitteeIndex]bitfield.Bitlist)
		}
		if _, exists := attested[data.Slot][data.Index]; !exists {
			if !exists {
				attested[data.Slot][data.Index] = bitfield.NewBitlist(attestation.AggregationBits.Len())
			}
		}

		// Calculate inclusion score.
		inclusionDistance := float64(blockProposal.Slot - data.Slot)
		for i := uint64(0); i < attestation.AggregationBits.Len(); i++ {
			if attestation.AggregationBits.BitAt(i) && !attested[attestation.Data.Slot][attestation.Data.Index].BitAt(i) {
				attestationScore += float64(0.75) + float64(0.25)/inclusionDistance
				if inclusionDistance == 1 {
					immediateAttestationScore += 1.0
				}
				attested[attestation.Data.Slot][attestation.Data.Index].SetBitAt(i, true)
			}
		}
	}

	attesterSlashingScore, proposerSlashingScore := scoreSlashings(blockProposal.Body.AttesterSlashings, blockProposal.Body.ProposerSlashings)

	// Scale scores by the distance between the proposal and parent slots.
	var scale uint64
	if blockProposal.Slot <= parentSlot {
		log.Warn().Uint64("slot", uint64(blockProposal.Slot)).Uint64("parent_slot", uint64(parentSlot)).Msg("Invalid parent slot for proposal")
		scale = 32
	} else {
		scale = uint64(blockProposal.Slot - parentSlot)
	}

	log.Trace().
		Uint64("slot", uint64(blockProposal.Slot)).
		Uint64("parent_slot", uint64(parentSlot)).
		Str("provider", name).
		Float64("immediate_attestations", immediateAttestationScore).
		Float64("attestations", attestationScore).
		Float64("proposer_slashings", proposerSlashingScore).
		Float64("attester_slashings", attesterSlashingScore).
		Uint64("scale", scale).
		Float64("total", attestationScore*float64(scale)+proposerSlashingScore+attesterSlashingScore).
		Msg("Scored phase 0 block")

	return attestationScore/float64(scale) + proposerSlashingScore + attesterSlashingScore
}

// scoreAltairBeaconBlockPropsal generates a score for an altair beacon block.
func (s *Service) scoreAltairBeaconBlockProposal(ctx context.Context,
	name string,
	parentSlot phase0.Slot,
	blockProposal *altair.BeaconBlock,
) float64 {
	attestationScore := float64(0)
	immediateAttestationScore := float64(0)

	// We need to avoid duplicates in attestations.
	// Map is attestation slot -> committee index -> validator committee index -> aggregate.
	attested := make(map[phase0.Slot]map[phase0.CommitteeIndex]bitfield.Bitlist)
	for _, attestation := range blockProposal.Body.Attestations {
		data := attestation.Data
		if _, exists := attested[data.Slot]; !exists {
			attested[data.Slot] = make(map[phase0.CommitteeIndex]bitfield.Bitlist)
		}
		if _, exists := attested[data.Slot][data.Index]; !exists {
			if !exists {
				attested[data.Slot][data.Index] = bitfield.NewBitlist(attestation.AggregationBits.Len())
			}
		}

		priorVotes, err := s.priorVotesForAttestation(ctx, attestation, blockProposal.ParentRoot)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to obtain prior votes for attestation; assuming no votes")
		}
		log.Trace().Str("prior_votes", fmt.Sprintf("%#x", priorVotes.Bytes())).Msg("Prior votes")

		votes := 0
		for i := uint64(0); i < attestation.AggregationBits.Len(); i++ {
			if attestation.AggregationBits.BitAt(i) {
				if attested[attestation.Data.Slot][attestation.Data.Index].BitAt(i) {
					// Already attested in this block; skip.
					continue
				}
				if priorVotes.BitAt(i) {
					// Attested in a previous block; skip.
					continue
				}
				votes++
				attested[attestation.Data.Slot][attestation.Data.Index].SetBitAt(i, true)
			}
		}

		// Now we know how many new votes are in this attestation we can score it.
		// We can calculate if the head vote is correct, but not target so for the
		// purposes of the calculation we assume that it is.
		switch blockProposal.Slot - attestation.Data.Slot {
		case 1:
			// If the attesation was for the past slot we know that the head vote
			// can only be correct if it matches the parent root in the block.
			score := float64(votes)
			if bytes.Equal(blockProposal.ParentRoot[:], attestation.Data.BeaconBlockRoot[:]) {
				score *= float64(s.timelySourceWeight+s.timelyTargetWeight+s.timelyHeadWeight) / float64(s.weightDenominator)
			} else {
				score *= float64(s.timelySourceWeight+s.timelyTargetWeight) / float64(s.weightDenominator)
			}
			attestationScore += score
			immediateAttestationScore += score
		case 2, 3, 4, 5:
			// Head vote is no longer timely; source and target counts.
			attestationScore += float64(votes) * float64(s.timelySourceWeight+s.timelyTargetWeight) / float64(s.weightDenominator)
		default:
			// Head and source votes are no longer timely; target counts.
			attestationScore += float64(votes) * float64(s.timelyTargetWeight) / float64(s.weightDenominator)
		}
	}

	attesterSlashingScore, proposerSlashingScore := scoreSlashings(blockProposal.Body.AttesterSlashings, blockProposal.Body.ProposerSlashings)

	// Add sync committee score.
	syncCommitteeScore := float64(blockProposal.Body.SyncAggregate.SyncCommitteeBits.Count()) * float64(s.syncRewardWeight) / float64(s.weightDenominator)

	log.Trace().
		Uint64("slot", uint64(blockProposal.Slot)).
		Uint64("parent_slot", uint64(parentSlot)).
		Str("provider", name).
		Float64("immediate_attestations", immediateAttestationScore).
		Float64("attestations", attestationScore).
		Float64("proposer_slashings", proposerSlashingScore).
		Float64("attester_slashings", attesterSlashingScore).
		Float64("sync_committee", syncCommitteeScore).
		Float64("total", attestationScore+proposerSlashingScore+attesterSlashingScore+syncCommitteeScore).
		Msg("Scored Altair block")

	return attestationScore + proposerSlashingScore + attesterSlashingScore + syncCommitteeScore
}

func scoreSlashings(attesterSlashings []*phase0.AttesterSlashing,
	proposerSlashings []*phase0.ProposerSlashing,
) (float64, float64) {
	// Slashing reward will be at most MAX_EFFECTIVE_BALANCE/WHISTLEBLOWER_REWARD_QUOTIENT,
	// which is 0.0625 Ether.
	// Individual attestation reward at 250K validators will be around 23,000 GWei, or .000023 Ether.
	// So we state that a single slashing event has the same weight as about 2,700 attestations.
	slashingWeight := float64(2700)

	// Add proposer slashing scores.
	proposerSlashingScore := float64(len(proposerSlashings)) * slashingWeight

	// Add attester slashing scores.
	indicesSlashed := 0
	for _, slashing := range attesterSlashings {
		indicesSlashed += len(intersection(slashing.Attestation1.AttestingIndices, slashing.Attestation2.AttestingIndices))
	}
	attesterSlashingScore := slashingWeight * float64(indicesSlashed)

	return attesterSlashingScore, proposerSlashingScore
}

func (s *Service) priorVotesForAttestation(ctx context.Context,
	attestation *phase0.Attestation,
	root phase0.Root,
) (
	bitfield.Bitlist,
	error,
) {
	var res bitfield.Bitlist
	var err error
	found := false
	s.priorBlocksMu.RLock()
	for {
		priorBlock, exists := s.priorBlocks[root]
		if !exists {
			// This means we do not have a parent block.
			break
		}
		if priorBlock.slot < attestation.Data.Slot-phase0.Slot(s.slotsPerEpoch) {
			// Block is too far back for its attestations to count.
			break
		}

		slotVotes, exists := priorBlock.votes[attestation.Data.Slot]
		if exists {
			votes, exists := slotVotes[attestation.Data.Index]
			if exists {
				if !found {
					res = bitfield.NewBitlist(votes.Len())
					found = true
				}
				res, err = res.Or(votes)
				if err != nil {
					return bitfield.Bitlist{}, err
				}
			}
		}

		root = priorBlock.parent
	}
	s.priorBlocksMu.RUnlock()

	if !found {
		// No prior votes found, return an empty list.
		return bitfield.NewBitlist(attestation.AggregationBits.Len()), nil
	}

	return res, nil
}

// intersection returns a list of items common between the two sets.
func intersection(set1 []uint64, set2 []uint64) []uint64 {
	sort.Slice(set1, func(i, j int) bool { return set1[i] < set1[j] })
	sort.Slice(set2, func(i, j int) bool { return set2[i] < set2[j] })
	res := make([]uint64, 0)

	set1Pos := 0
	set2Pos := 0
	for set1Pos < len(set1) && set2Pos < len(set2) {
		switch {
		case set1[set1Pos] < set2[set2Pos]:
			set1Pos++
		case set2[set2Pos] < set1[set1Pos]:
			set2Pos++
		default:
			res = append(res, set1[set1Pos])
			set1Pos++
			set2Pos++
		}
	}

	return res
}
