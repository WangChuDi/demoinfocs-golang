package demoinfocs

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/msgs2"
)

// userCmdButtonRing keeps the same command-number validation as the full
// decoder, but stores only the one value needed by PlayerButtonsStateUpdate.
type userCmdButtonRing struct {
	entries [userCmdRingSize]userCmdButtonRingEntry
}

type userCmdButtonRingEntry struct {
	commandNumber int32
	buttons       uint64
	valid         bool
}

func (r *userCmdButtonRing) insert(commandNumber int32, buttons uint64) {
	r.entries[userCmdRingIndex(commandNumber)] = userCmdButtonRingEntry{
		commandNumber: commandNumber,
		buttons:       buttons,
		valid:         true,
	}
}

func (r *userCmdButtonRing) get(commandNumber int32) (uint64, bool) {
	entry := r.entries[userCmdRingIndex(commandNumber)]
	if !entry.valid || entry.commandNumber != commandNumber {
		return 0, false
	}

	return entry.buttons, true
}

type userCmdButtonPlayerState struct {
	ring                 userCmdButtonRing
	currentCommandNumber int32
	hasCurrentCommand    bool
}

// The buttonstate1 path is CSGOUserCmdPB.base.buttons_pb.buttonstate1.
var userCmdButtonsPath = [...]protowire.Number{1, 3, 1}

// mergeUserCmdButtons extracts only buttonstate1 from a full snapshot or delta.
// It skips all other fields without proto.Unmarshal or allocation. Wire type 7
// resets a target field to its protobuf default, including when the marker is on
// base or buttons_pb.
//
func mergeUserCmdButtons(data []byte, level int, buttons *uint64) error {
	_, err := mergeUserCmdButtonsWithPresence(data, level, buttons)
	return err
}

// The presence flag distinguishes an absolute button value (including an
// explicit zero or parent reset) from an unrelated delta that needs a baseline.
//
//nolint:gocognit,funlen
func mergeUserCmdButtonsWithPresence(data []byte, level int, buttons *uint64) (bool, error) {
	if level >= len(userCmdButtonsPath) {
		return false, errors.New("invalid user command button path")
	}

	target := userCmdButtonsPath[level]
	present := false

	for len(data) > 0 {
		num, typ, tagLen := protowire.ConsumeTag(data)
		if tagLen < 0 {
			return false, fmt.Errorf("failed to read user command button tag: %w", protowire.ParseError(tagLen))
		}

		if num == 0 {
			return false, errors.New("user command button field number must be non-zero")
		}

		if typ == wireTypeReset {
			if num == target {
				*buttons = 0
				present = true
			}

			data = data[tagLen:]

			continue
		}

		if num != target {
			valueLen := protowire.ConsumeFieldValue(num, typ, data[tagLen:])
			if valueLen < 0 {
				return false, fmt.Errorf("failed to skip user command field: %w", protowire.ParseError(valueLen))
			}

			data = data[tagLen+valueLen:]

			continue
		}

		if level == len(userCmdButtonsPath)-1 {
			if typ != protowire.VarintType {
				return false, fmt.Errorf("buttonstate1 uses unexpected wire type %d", typ)
			}

			value, valueLen := protowire.ConsumeVarint(data[tagLen:])
			if valueLen < 0 {
				return false, fmt.Errorf("failed to read buttonstate1: %w", protowire.ParseError(valueLen))
			}

			*buttons = value
			present = true
			data = data[tagLen+valueLen:]

			continue
		}

		if typ != protowire.BytesType {
			return false, fmt.Errorf("user command button message uses unexpected wire type %d", typ)
		}

		value, valueLen := protowire.ConsumeBytes(data[tagLen:])
		if valueLen < 0 {
			return false, fmt.Errorf("failed to read user command button message: %w", protowire.ParseError(valueLen))
		}

		nestedPresent, err := mergeUserCmdButtonsWithPresence(value, level+1, buttons)
		if err != nil {
			return false, err
		}
		present = present || nestedPresent

		data = data[tagLen+valueLen:]
	}

	return present, nil
}

//nolint:gocognit,nestif,funlen
func (p *parser) handleUserCommandButtons(m *msgs2.CSVCMsg_UserCommands) {
	p.hasUserCmdMessages = true

	for _, cmd := range m.Commands {
		if cmd == nil || cmd.CmdNumber == nil {
			continue
		}

		slot := cmd.GetPlayerSlot()
		if slot < 0 {
			continue
		}

		state := p.userCmdButtonStates[slot]
		if state == nil {
			state = &userCmdButtonPlayerState{}
			p.userCmdButtonStates[slot] = state
		}

		var buttons uint64
		hasBaseline := false
		if data := cmd.GetData(); len(data) > 0 {
			if err := mergeUserCmdButtons(data, 0, &buttons); err != nil {
				p.msgDispatcher.Dispatch(events.ParserWarn{
					Message: err.Error(),
					Type:    events.WarnTypeUserCommandDeltaDecodeFailed,
				})

				continue
			}
			hasBaseline = true
		} else if len(cmd.GetDeltaData()) > 0 {
			if state.hasCurrentCommand {
				requestedBaseline := state.currentCommandNumber
				if cmd.GetCmdNumber() < requestedBaseline {
					p.msgDispatcher.Dispatch(events.ParserWarn{
						Message: fmt.Sprintf("user command %d precedes its current baseline %d for player slot %d", cmd.GetCmdNumber(), requestedBaseline, slot),
						Type:    events.WarnTypeUserCommandBaselineMismatch,
					})

					continue
				}

				var found bool

				buttons, found = state.ring.get(requestedBaseline)
				if !found {
					p.msgDispatcher.Dispatch(events.ParserWarn{
						Message: fmt.Sprintf("user command %d has a baseline ring mismatch for player slot %d (requested %d)", cmd.GetCmdNumber(), slot, requestedBaseline),
						Type:    events.WarnTypeUserCommandBaselineMismatch,
					})

					continue
				}
				hasBaseline = true
			}
		} else {
			continue
		}

		if deltaData := cmd.GetDeltaData(); len(deltaData) > 0 {
			present, err := mergeUserCmdButtonsWithPresence(deltaData, 0, &buttons)
			if err != nil {
				p.msgDispatcher.Dispatch(events.ParserWarn{
					Message: err.Error(),
					Type:    events.WarnTypeUserCommandDeltaDecodeFailed,
				})

				continue
			}
			// A delta can establish the button-only baseline without a full
			// user command when it explicitly replaces or resets buttonstate1.
			if !hasBaseline && !present {
				p.msgDispatcher.Dispatch(events.ParserWarn{
					Message: fmt.Sprintf("user command %d has no baseline for player slot %d", cmd.GetCmdNumber(), slot),
					Type:    events.WarnTypeUserCommandBaselineMissing,
				})
				continue
			}
		}

		commandNumber := cmd.GetCmdNumber()
		state.ring.insert(commandNumber, buttons)
		state.currentCommandNumber = commandNumber
		state.hasCurrentCommand = true

		// Player slots map to controller entities: slot N is entity N+1
		// (getOrCreatePlayerFromControllerEntity relies on the same convention).
		player := p.gameState.playersByEntityID[int(slot)+1]
		if player == nil {
			continue
		}

		if player.ButtonsPressedState != buttons {
			player.ButtonsPressedState = buttons
			p.eventDispatcher.Dispatch(events.PlayerButtonsStateUpdate{
				Player:       player,
				ButtonsState: buttons,
			})
		}
	}
}
