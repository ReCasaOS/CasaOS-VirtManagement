package libvirt

import golibvirt "github.com/digitalocean/go-libvirt"

// States as the API names them.
const (
	StateCreating     = "creating"
	StateRunning      = "running"
	StatePaused       = "paused"
	StateShuttingDown = "shutting-down"
	StateShutOff      = "shut-off"
	StateCrashed      = "crashed"
	StateSuspended    = "suspended"
	StateUnknown      = "unknown"
)

// StateName maps a libvirt domain state to the API's name for it.
func StateName(state golibvirt.DomainState) string {
	switch state {
	case golibvirt.DomainRunning, golibvirt.DomainBlocked:
		return StateRunning
	case golibvirt.DomainPaused:
		return StatePaused
	case golibvirt.DomainShutdown:
		return StateShuttingDown
	case golibvirt.DomainShutoff:
		return StateShutOff
	case golibvirt.DomainCrashed:
		return StateCrashed
	case golibvirt.DomainPmsuspended:
		return StateSuspended
	default:
		return StateUnknown
	}
}

// EventName maps a libvirt lifecycle event to a word for vm:state-changed.
func EventName(event golibvirt.DomainEventType) string {
	switch event {
	case golibvirt.DomainEventDefined:
		return "defined"
	case golibvirt.DomainEventUndefined:
		return "undefined"
	case golibvirt.DomainEventStarted:
		return "started"
	case golibvirt.DomainEventSuspended:
		return "suspended"
	case golibvirt.DomainEventResumed:
		return "resumed"
	case golibvirt.DomainEventStopped:
		return "stopped"
	case golibvirt.DomainEventShutdown:
		return "shutdown"
	case golibvirt.DomainEventPmsuspended:
		return "pmsuspended"
	case golibvirt.DomainEventCrashed:
		return "crashed"
	default:
		return "unknown"
	}
}
