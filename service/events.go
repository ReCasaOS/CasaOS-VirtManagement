package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/ReCasaOS/CasaOS-VirtManagement/common"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/libvirt"
	"github.com/ReCasaOS/CasaOS-VirtManagement/pkg/vmstats"
	golibvirt "github.com/digitalocean/go-libvirt"
	"go.uber.org/zap"
)

// Event names start with "vm:": the dashboard receives every event by name
// alone (socket.io), so they must not collide with app:* and the others.
const (
	EventStateChanged = "vm:state-changed"
	EventCreateEnd    = "vm:create-end"
	EventCreateError  = "vm:create-error"
	EventDeleteEnd    = "vm:delete-end"

	PropertyID           = "vm:id"
	PropertyName         = "vm:name"
	PropertyTitle        = "vm:title"
	PropertyState        = "vm:state"
	PropertyEvent        = "vm:event"
	PropertyDisksDeleted = "vm:disks-deleted"
	PropertyMessage      = "message"

	// resource alerts (cpu_high, mem_high, vm_crashed), read by the core
	EventResourceFiring   = vmstats.EventFiring
	EventResourceResolved = vmstats.EventResolved
)

var resourceProperties = []string{"item", "name", "kind", "situation", "value", "since", "source", "minutes"}

func eventType(name string, properties ...string) external.EventType {
	eventType := external.EventType{Name: name, SourceID: common.VirtManagementServiceName}
	for _, property := range properties {
		eventType.PropertyTypeList = append(eventType.PropertyTypeList, external.PropertyType{Name: property})
	}

	return eventType
}

var EventTypes = []external.EventType{
	eventType(EventStateChanged, PropertyID, PropertyName, PropertyTitle, PropertyState, PropertyEvent),
	eventType(EventCreateEnd, PropertyID, PropertyName, PropertyTitle),
	eventType(EventCreateError, PropertyID, PropertyName, PropertyTitle, PropertyMessage),
	eventType(EventDeleteEnd, PropertyID, PropertyName, PropertyTitle, PropertyDisksDeleted),
	eventType(EventResourceFiring, resourceProperties...),
	eventType(EventResourceResolved, resourceProperties...),
}

// Events publishes to the MessageBus over its unix socket.
type Events struct {
	runtimePath string
	lifecycle   atomic.Pointer[func(vmstats.LifecycleEvent)]
}

// OnLifecycle has f called with every lifecycle event WatchLifecycle reads,
// before its own RPCs; f must not block.
func (e *Events) OnLifecycle(f func(vmstats.LifecycleEvent)) { e.lifecycle.Store(&f) }

func lifecycleEventOf(m golibvirt.DomainEventLifecycleMsg) vmstats.LifecycleEvent {
	return vmstats.LifecycleEvent{UUID: uuidString(m.Dom.UUID), Name: m.Dom.Name, Event: golibvirt.DomainEventType(m.Event), Detail: m.Detail}
}

func NewEvents(runtimePath string) *Events {
	return &Events{runtimePath: runtimePath}
}

type eventTypeJSON struct {
	SourceID         string             `json:"sourceID"`
	Name             string             `json:"name"`
	PropertyTypeList []propertyTypeJSON `json:"propertyTypeList"`
}

type propertyTypeJSON struct {
	Name string `json:"name"`
}

// Register makes the event types discoverable on the MessageBus. Registering
// them again after a restart is answered 409, which is fine.
func (e *Events) Register(ctx context.Context) error {
	body := make([]eventTypeJSON, 0, len(EventTypes))
	for _, t := range EventTypes {
		j := eventTypeJSON{SourceID: t.SourceID, Name: t.Name}
		for _, p := range t.PropertyTypeList {
			j.PropertyTypeList = append(j.PropertyTypeList, propertyTypeJSON{Name: p.Name})
		}
		body = append(body, j)
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	socketPath := external.MessageBusSocketPath(e.runtimePath)
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
			DisableKeepAlives: true,
		},
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/v2/message_bus/event_type", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("message bus answered %s: %s", resp.Status, msg)
	}

	return nil
}

// Publish sends an event. A failure is logged, never returned: an event that
// does not arrive must not fail the operation it reports.
func (e *Events) Publish(name string, properties map[string]string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := external.PublishEventInSocket(ctx, e.runtimePath, common.VirtManagementServiceName, name, properties)
	if err != nil {
		logger.Error("error when publishing an event", zap.String("event", name), zap.Error(err))
		return
	}

	if resp.StatusCode != http.StatusOK {
		logger.Error("error when publishing an event", zap.String("event", name), zap.String("status", resp.Status))
	}
}

// WatchLifecycle publishes vm:state-changed for every libvirt lifecycle event,
// so a VM stopped from inside the guest or with virsh is reported too. It
// returns when ctx is done or the connection is lost; Conn calls it again on
// the next connection.
func (e *Events) WatchLifecycle(ctx context.Context, l *golibvirt.Libvirt) {
	events, err := l.LifecycleEvents(ctx)
	if err != nil {
		logger.Error("error when subscribing to libvirt lifecycle events", zap.Error(err))
		return
	}

	for ev := range events {
		if f := e.lifecycle.Load(); f != nil {
			(*f)(lifecycleEventOf(ev))
		}

		properties := map[string]string{
			PropertyID:    uuidString(ev.Dom.UUID),
			PropertyName:  ev.Dom.Name,
			PropertyEvent: libvirt.EventName(golibvirt.DomainEventType(ev.Event)),
		}

		if golibvirt.DomainEventType(ev.Event) == golibvirt.DomainEventUndefined {
			properties[PropertyState] = libvirt.StateShutOff
		} else if state, _, err := l.DomainGetState(ev.Dom, 0); err == nil {
			properties[PropertyState] = libvirt.StateName(golibvirt.DomainState(state))
		}

		if xmlDesc, err := l.DomainGetXMLDesc(ev.Dom, 0); err == nil {
			if d, err := libvirt.ParseDomainXML(xmlDesc); err == nil {
				properties[PropertyTitle] = titleOf(d)
			}
		}

		e.Publish(EventStateChanged, properties)
	}
}
