package service

import "github.com/ReCasaOS/CasaOS-VirtManagement/pkg/vmstats"

// The VM statistics service lives in pkg/vmstats, which builds and tests on
// any OS; these are its names here.
type (
	Stats          = vmstats.Stats
	StatsOptions   = vmstats.Options
	StatsSource    = vmstats.Source
	StatsPublisher = vmstats.Publisher
	StatsItem      = vmstats.Item
	StatsSeries    = vmstats.Series
	StatsScale     = vmstats.Scale
	StatsAlert     = vmstats.Alert
)

var (
	NewStats            = vmstats.New
	NewConnSource       = vmstats.NewConnSource
	ErrUnknownItem      = vmstats.ErrUnknownItem
	ErrStatsUnavailable = vmstats.ErrUnavailable
)

var _ StatsPublisher = (*Events)(nil)
