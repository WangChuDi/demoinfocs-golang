package demoinfocs

import (
	"bytes"
	"testing"

	"github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v4/pkg/demoinfocs/msgs2"
	"github.com/stretchr/testify/require"
)

func newButtonBaselineTestParser(t *testing.T) (*parser, *common.Player, *[]uint64, *[]events.WarnType) {
	t.Helper()
	config := DefaultParserConfig
	config.MsgQueueBufferSize = -1
	p := NewParserWithConfig(bytes.NewReader(make([]byte, 8)), config).(*parser)
	t.Cleanup(func() { _ = p.Close() })
	player := &common.Player{ButtonsPressedState: 123}
	p.gameState.playersByEntityID[1] = player
	states := []uint64{}
	warnings := []events.WarnType{}
	p.RegisterEventHandler(func(e events.PlayerButtonsStateUpdate) { states = append(states, e.ButtonsState) })
	p.RegisterNetMessageHandler(func(e events.ParserWarn) { warnings = append(warnings, e.Type) })
	return p, player, &states, &warnings
}

func sendButtonDelta(p *parser, number int32, data []byte) {
	p.handleUserCommandButtons(&msgs2.CSVCMsg_UserCommands{Commands: []*msgs2.CMsgServerUserCmd{{
		PlayerSlot: proto32(0), CmdNumber: proto32(number), DeltaData: data,
	}}})
}

func TestUserCmdButtonsInitialDeltaCanEstablishButtonBaseline(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want uint64
	}{
		{"absolute", bytesField(1, bytesField(3, varintField(1, 512))), 512},
		{"explicit zero", bytesField(1, bytesField(3, varintField(1, 0))), 0},
		{"64 bit", bytesField(1, bytesField(3, varintField(1, 1<<33))), 1 << 33},
		{"button reset", bytesField(1, bytesField(3, resetMarker(1))), 0},
		{"buttons parent reset", bytesField(1, resetMarker(3)), 0},
		{"base parent reset", resetMarker(1), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, player, states, warnings := newButtonBaselineTestParser(t)
			sendButtonDelta(p, 100, tc.data)
			require.True(t, p.userCmdButtonStates[0].hasCurrentCommand)
			require.Equal(t, tc.want, player.ButtonsPressedState)
			require.Equal(t, []uint64{tc.want}, *states)
			require.Empty(t, *warnings)
			// Subsequent deltas without buttons inherit the established state.
			sendButtonDelta(p, 101, bytesField(1, varintField(2, 101)))
			got, found := p.userCmdButtonStates[0].ring.get(101)
			require.True(t, found)
			require.Equal(t, tc.want, got)
			require.Len(t, *states, 1)
		})
	}
}

func TestUserCmdButtonsUnrelatedInitialDeltaRemainsUnknown(t *testing.T) {
	p, player, states, warnings := newButtonBaselineTestParser(t)
	sendButtonDelta(p, 100, bytesField(1, varintField(2, 100)))
	require.False(t, p.userCmdButtonStates[0].hasCurrentCommand)
	require.EqualValues(t, 123, player.ButtonsPressedState)
	require.Empty(t, *states)
	require.Equal(t, []events.WarnType{events.WarnTypeUserCommandBaselineMissing}, *warnings)
	_, found := p.userCmdButtonStates[0].ring.get(100)
	require.False(t, found)
	sendButtonDelta(p, 101, bytesField(1, bytesField(3, varintField(1, 1))))
	require.Equal(t, []uint64{1}, *states)
	require.EqualValues(t, 101, p.userCmdButtonStates[0].currentCommandNumber)
}

func TestUserCmdButtonsMalformedInitialDeltaDoesNotCommitPartialState(t *testing.T) {
	p, player, states, warnings := newButtonBaselineTestParser(t)
	data := append(bytesField(1, bytesField(3, varintField(1, 512))), 0x80)
	sendButtonDelta(p, 100, data)
	require.False(t, p.userCmdButtonStates[0].hasCurrentCommand)
	require.EqualValues(t, 123, player.ButtonsPressedState)
	require.Empty(t, *states)
	require.Equal(t, []events.WarnType{events.WarnTypeUserCommandDeltaDecodeFailed}, *warnings)
	sendButtonDelta(p, 101, resetMarker(1))
	require.Equal(t, []uint64{0}, *states)
}

func TestUserCmdButtonsPreservesCommandAndRingValidation(t *testing.T) {
	p, player, states, warnings := newButtonBaselineTestParser(t)
	sendButtonDelta(p, 100, bytesField(1, bytesField(3, varintField(1, 512))))
	sendButtonDelta(p, 99, bytesField(1, bytesField(3, varintField(1, 1))))
	require.EqualValues(t, 512, player.ButtonsPressedState)
	require.EqualValues(t, 100, p.userCmdButtonStates[0].currentCommandNumber)
	p.userCmdButtonStates[0].ring.insert(100+userCmdRingSize, 8)
	sendButtonDelta(p, 101, bytesField(1, bytesField(3, varintField(1, 1))))
	require.EqualValues(t, 512, player.ButtonsPressedState)
	require.Equal(t, []events.WarnType{
		events.WarnTypeUserCommandBaselineMismatch, events.WarnTypeUserCommandBaselineMismatch,
	}, *warnings)
	// A genuine full snapshot still resynchronizes the command state.
	p.handleUserCommandButtons(&msgs2.CSVCMsg_UserCommands{Commands: []*msgs2.CMsgServerUserCmd{{
		PlayerSlot: proto32(0), CmdNumber: proto32(200), Data: bytesField(1, bytesField(3, varintField(1, 8))),
	}}})
	sendButtonDelta(p, 201, bytesField(1, varintField(2, 201)))
	require.Equal(t, []uint64{512, 8}, *states)
	require.EqualValues(t, 201, p.userCmdButtonStates[0].currentCommandNumber)
}
