package pprof

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/cespare/xxhash/v2"
	"github.com/pyroscope-io/pyroscope/pkg/convert/pprof"
	"github.com/pyroscope-io/pyroscope/pkg/storage/metadata"
	"github.com/pyroscope-io/pyroscope/pkg/storage/segment"
	"github.com/pyroscope-io/pyroscope/pkg/storage/tree"
	"github.com/pyroscope-io/pyroscope/pkg/util/form"

	"github.com/alibaba/ilogtail/helper/profile"
	"github.com/alibaba/ilogtail/pkg/logger"
	"github.com/alibaba/ilogtail/pkg/models"
	"github.com/alibaba/ilogtail/pkg/protocol"
)

const (
	formFieldProfile, formFieldSampleTypeConfig = "profile", "sample_type_config"
)

var DefaultSampleTypeMapping = map[string]*tree.SampleTypeConfig{
	"cpu": {
		Units:   metadata.Units(profile.NanosecondsUnit),
		Sampled: true,
	},
	"inuse_objects": {
		Units:       metadata.ObjectsUnits,
		Aggregation: "avg",
	},
	"alloc_objects": {
		Units:      metadata.ObjectsUnits,
		Cumulative: true,
	},
	"inuse_space": {
		Units:       metadata.BytesUnits,
		Aggregation: "avg",
	},
	"alloc_space": {
		Units:      metadata.BytesUnits,
		Cumulative: true,
	},
	"goroutine": {
		DisplayName: "goroutines",
		Units:       metadata.GoroutinesUnits,
		Aggregation: "avg",
	},
	"contentions": {
		DisplayName: "mutex_count",
		Units:       metadata.LockSamplesUnits,
		Cumulative:  true,
	},
	"delay": {
		DisplayName: "mutex_duration",
		Units:       metadata.LockNanosecondsUnits,
		Cumulative:  true,
	},
}

type RawProfile struct {
	RawData             []byte
	FormDataContentType string
	profile             []byte
	sampleTypeConfig    map[string]*tree.SampleTypeConfig

	logs  []*protocol.Log             // v1 result
	group *models.PipelineGroupEvents // v2 result
}

func (r *RawProfile) Parse(ctx context.Context, meta *profile.Meta) (logs []*protocol.Log, err error) {
	cb := r.extraceProfileV1(meta)
	if err = r.doParse(ctx, meta, cb); err != nil {
		return nil, err
	}
	logs = r.logs
	r.logs = nil
	return
}

func (r *RawProfile) ParseV2(ctx context.Context, meta *profile.Meta) (groups *models.PipelineGroupEvents, err error) {
	groups = new(models.PipelineGroupEvents)
	r.group = groups
	cb := r.extraceProfileV2(meta)
	if err = r.doParse(ctx, meta, cb); err != nil {
		return nil, err
	}
	r.group = nil
	return
}

func (r *RawProfile) doParse(ctx context.Context, meta *profile.Meta, cb profile.CallbackFunc) error {
	if err := r.extractProfileRaw(); err != nil {
		return fmt.Errorf("cannot extract profile: %w", err)
	}
	if len(r.profile) == 0 {
		return errors.New("empty profile")
	}

	if meta.SampleRate > 0 {
		meta.Key.Labels()["_sample_rate_"] = strconv.FormatUint(uint64(meta.SampleRate), 10)
	}
	return pprof.DecodePool(bytes.NewReader(r.profile), func(tf *tree.Profile) error {
		if logger.DebugFlag() {
			var keys []string
			for k := range r.sampleTypeConfig {
				keys = append(keys, k)
			}
			logger.Debug(ctx, "pprof default sampleTypeConfig: ", r.sampleTypeConfig == nil, "config:", strings.Join(keys, ","))
		}
		if r.sampleTypeConfig == nil {
			r.sampleTypeConfig = DefaultSampleTypeMapping
		}
		p := Parser{
			stackFrameFormatter: Formatter{},
			sampleTypesFilter:   filterKnownSamples(r.sampleTypeConfig),
			sampleTypes:         r.sampleTypeConfig,
		}

		if err := extractLogs(ctx, tf, p, meta, cb); err != nil {
			return err
		}
		return nil
	})
}

func extractLogs(ctx context.Context, tp *tree.Profile, p Parser, meta *profile.Meta, cb profile.CallbackFunc) error {

	stackMap := make(map[uint64]*profile.Stack)
	valMap := make(map[uint64][]uint64)
	labelMap := make(map[uint64]map[string]string)
	typeMap := make(map[uint64][]string)
	unitMap := make(map[uint64][]string)
	aggtypeMap := make(map[uint64][]string)

	if len(tp.SampleType) > 0 {
		meta.Units = profile.Units(tp.StringTable[tp.SampleType[0].Type])
	}

	err := p.iterate(tp, func(vt *tree.ValueType, tl tree.Labels, t *tree.Tree) (keep bool, err error) {
		if len(tp.StringTable) <= int(vt.Type) || len(tp.StringTable) <= int(vt.Unit) {
			return true, errors.New("invalid type or unit")
		}
		stype := tp.StringTable[vt.Type]
		sunit := tp.StringTable[vt.Unit]

		t.IterateStacks(func(name string, self uint64, stack []string) {
			if name == "" {
				return
			}
			id := xxhash.Sum64String(strings.Join(stack, ""))
			stackMap[id] = &profile.Stack{
				Name:  name,
				Stack: stack[1:],
			}
			aggtypeMap[id] = append(aggtypeMap[id], p.getAggregationType(stype, string(meta.AggregationType)))
			typeMap[id] = append(typeMap[id], p.getDisplayName(stype))
			unitMap[id] = append(unitMap[id], sunit)
			valMap[id] = append(valMap[id], self)
			labelMap[id] = buildKey(meta.Key.Labels(), tl, tp.StringTable).Labels()
		})
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("iterate profile tree error: %w", err)
	}
	for id, fs := range stackMap {
		if len(valMap[id]) == 0 || len(typeMap[id]) == 0 || len(unitMap[id]) == 0 || len(aggtypeMap[id]) == 0 {
			logger.Warning(ctx, "PPROF_PROFILE_ALARM", "stack don't have enough meta or values", fs)
			continue
		}
		cb(id, fs, valMap[id], typeMap[id], unitMap[id], aggtypeMap[id], tp.GetTimeNanos(), tp.GetTimeNanos()+tp.GetDurationNanos(), labelMap[id])
	}
	return nil
}

func (r *RawProfile) extraceProfileV2(meta *profile.Meta) profile.CallbackFunc {
	if r.group.Group == nil {
		r.group.Group = models.NewGroup(models.NewMetadata(), models.NewTags())
	}
	profileID := profile.GetProfileID(meta)
	return func(id uint64, stack *profile.Stack, vals []uint64, types, units, aggs []string, startTime, endTime int64, labels map[string]string) {
		var values models.ProfileValues
		for i, val := range vals {
			values = append(values, models.NewProfileValue(types[i], units[i], aggs[i], float64(val)))
		}
		newProfile := models.NewProfile(stack.Name, strconv.FormatUint(id, 16),
			profileID,
			"CallStack",
			meta.SpyName,
			profile.DetectProfileType(types[0]),
			stack.Stack, startTime, endTime, models.NewTagsWithMap(labels), values)
		r.group.Events = append(r.group.Events, newProfile)
	}
}

func (r *RawProfile) extraceProfileV1(meta *profile.Meta) profile.CallbackFunc {
	profileIDStr := profile.GetProfileID(meta)
	return func(id uint64, stack *profile.Stack, vals []uint64, types, units, aggs []string, startTime, endTime int64, labels map[string]string) {
		b, _ := json.Marshal(labels)
		var content []*protocol.Log_Content
		content = append(content,
			&protocol.Log_Content{
				Key:   "name",
				Value: stack.Name,
			},
			&protocol.Log_Content{
				Key:   "stack",
				Value: strings.Join(stack.Stack, "\n"),
			},
			&protocol.Log_Content{
				Key:   "stackID",
				Value: strconv.FormatUint(id, 16),
			},
			&protocol.Log_Content{
				Key:   "language",
				Value: meta.SpyName,
			},
			&protocol.Log_Content{
				Key:   "type",
				Value: profile.DetectProfileType(types[0]).String(),
			},
			&protocol.Log_Content{
				Key:   "units",
				Value: strings.Join(units, ","),
			},
			&protocol.Log_Content{
				Key:   "valueTypes",
				Value: strings.Join(types, ","),
			},
			&protocol.Log_Content{
				Key:   "aggTypes",
				Value: strings.Join(aggs, ","),
			},
			&protocol.Log_Content{
				Key:   "dataType",
				Value: "CallStack",
			},
			&protocol.Log_Content{
				Key:   "durationNs",
				Value: strconv.FormatInt(endTime-startTime, 10),
			},
			&protocol.Log_Content{
				Key:   "profileID",
				Value: profileIDStr,
			},
			&protocol.Log_Content{
				Key:   "labels",
				Value: string(b),
			},
		)
		for i, v := range vals {
			content = append(content, &protocol.Log_Content{
				Key:   fmt.Sprintf("value_%d", i),
				Value: strconv.FormatUint(v, 10),
			})
		}
		log := &protocol.Log{
			Time:     uint32(meta.StartTime.Unix()),
			Contents: content,
		}
		r.logs = append(r.logs, log)
	}
}

func buildKey(appLabels map[string]string, labels tree.Labels, table []string) *segment.Key {
	finalLabels := map[string]string{}
	for k, v := range appLabels {
		finalLabels[k] = v
	}
	for _, v := range labels {
		ks := table[v.Key]
		if ks == "" {
			continue
		}
		vs := table[v.Str]
		if vs == "" {
			continue
		}
		finalLabels[ks] = vs
	}
	return segment.NewKey(finalLabels)
}

func (r *RawProfile) extractProfileRaw() error {
	if r.FormDataContentType == "" {
		r.profile = r.RawData
		return nil
	}
	boundary, err := form.ParseBoundary(r.FormDataContentType)
	if err != nil {
		return err
	}
	f, err := multipart.NewReader(bytes.NewReader(r.RawData), boundary).ReadForm(32 << 20)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.RemoveAll()
	}()

	if r.profile, err = form.ReadField(f, formFieldProfile); err != nil {
		return err
	}
	if c, err := form.ReadField(f, formFieldSampleTypeConfig); err != nil {
		return err
	} else if c != nil {
		var config map[string]*tree.SampleTypeConfig
		if err = json.Unmarshal(c, &config); err != nil {
			return err
		}
		r.sampleTypeConfig = config
	}
	return nil
}
