// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package synthexec

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/elastic/beats/v7/heartbeat/monitors/logger"
	"github.com/elastic/beats/v7/heartbeat/monitors/wrappers"
	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/beat/events"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/logp"
	"github.com/elastic/beats/v7/libbeat/processors/add_data_stream"
	"github.com/elastic/go-lookslike"
	"github.com/elastic/go-lookslike/testslike"
	"github.com/elastic/go-lookslike/validator"
)

func makeStepEvent(typ string, ts float64, name string, index int, status string, urlstr string, err *SynthError) *SynthEvent {
	return &SynthEvent{
		Type:                 typ,
		TimestampEpochMicros: 1000 + ts,
		PackageVersion:       "1.0.0",
		Step:                 &Step{Name: name, Index: index, Status: status},
		Error:                err,
		Payload:              common.MapStr{},
		URL:                  urlstr,
	}
}

func TestJourneyEnricher(t *testing.T) {
	var stdFields = StdSuiteFields{
		Id:       "mysuite",
		Name:     "mysuite",
		Type:     "browser",
		IsInline: false,
	}
	journey := &Journey{
		Name: "A Journey Name",
		Id:   "my-journey-id",
	}
	syntherr := &SynthError{
		Message: "my-errmsg",
		Name:    "my-errname",
		Stack:   "my\nerr\nstack",
	}
	otherErr := &SynthError{
		Message: "last-errmsg",
		Name:    "last-errname",
		Stack:   "last\nerr\nstack",
	}
	journeyStart := &SynthEvent{
		Type:                 JourneyStart,
		TimestampEpochMicros: 1000,
		PackageVersion:       "1.0.0",
		Journey:              journey,
		Payload:              common.MapStr{},
	}
	journeyEnd := &SynthEvent{
		Type:                 JourneyEnd,
		TimestampEpochMicros: 2000,
		PackageVersion:       "1.0.0",
		Journey:              journey,
		Payload:              common.MapStr{},
	}
	url1 := "http://example.net/url1"
	url2 := "http://example.net/url2"
	url3 := "http://example.net/url3"

	synthEvents := []*SynthEvent{
		journeyStart,
		makeStepEvent("step/start", 10, "Step1", 1, "succeeded", "", nil),
		makeStepEvent("step/end", 20, "Step1", 1, "", url1, nil),
		makeStepEvent("step/start", 21, "Step2", 2, "", "", nil),
		makeStepEvent("step/end", 30, "Step2", 2, "failed", url2, syntherr),
		makeStepEvent("step/start", 31, "Step3", 3, "", "", nil),
		makeStepEvent("step/end", 40, "Step3", 3, "", url3, otherErr),
		journeyEnd,
	}

	suiteValidator := func() validator.Validator {
		return lookslike.MustCompile(common.MapStr{
			"suite.id":     stdFields.Id,
			"suite.name":   stdFields.Name,
			"monitor.id":   fmt.Sprintf("%s-%s", stdFields.Id, journey.Id),
			"monitor.name": fmt.Sprintf("%s - %s", stdFields.Name, journey.Name),
			"monitor.type": stdFields.Type,
		})
	}
	inlineValidator := func() validator.Validator {
		return lookslike.MustCompile(common.MapStr{
			"monitor.id":   stdFields.Id,
			"monitor.name": stdFields.Name,
			"monitor.type": stdFields.Type,
		})
	}
	commonValidator := func(se *SynthEvent) validator.Validator {
		var v []validator.Validator

		// We need an expectation for each input plus a final
		// expectation for the summary which comes on the nil data.

		if se.Type != JourneyEnd {
			// Test that the created event includes the mapped
			// version of the event
			v = append(v, lookslike.MustCompile(se.ToMap()))
		} else {
			u, _ := url.Parse(url1)
			// journey end gets a summary
			v = append(v, lookslike.MustCompile(common.MapStr{
				"synthetics.type":     "heartbeat/summary",
				"url":                 wrappers.URLFields(u),
				"monitor.duration.us": int64(journeyEnd.Timestamp().Sub(journeyStart.Timestamp()) / time.Microsecond),
			}))
		}
		return lookslike.Compose(v...)
	}

	je := &journeyEnricher{}
	check := func(t *testing.T, se *SynthEvent, ssf StdSuiteFields) {
		e := &beat.Event{}
		t.Run(fmt.Sprintf("event: %s", se.Type), func(t *testing.T) {
			enrichErr := je.enrich(e, se, ssf)
			if se.Error != nil {
				require.Equal(t, stepError(se.Error), enrichErr)
			}
			if ssf.IsInline {
				sv, _ := e.Fields.GetValue("suite")
				require.Nil(t, sv)
				testslike.Test(t, inlineValidator(), e.Fields)
			} else {
				testslike.Test(t, suiteValidator(), e.Fields)
			}
			testslike.Test(t, commonValidator(se), e.Fields)

			require.Equal(t, se.Timestamp().Unix(), e.Timestamp.Unix())
		})
	}

	tests := []struct {
		name     string
		isInline bool
	}{
		{
			name:     "suite monitor",
			isInline: false,
		},
		{
			name:     "inline monitor",
			isInline: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdFields.IsInline = tt.isInline
			for _, se := range synthEvents {
				check(t, se, stdFields)
			}
		})
	}
}

func TestEnrichConsoleSynthEvents(t *testing.T) {
	tests := []struct {
		name  string
		je    *journeyEnricher
		se    *SynthEvent
		check func(t *testing.T, e *beat.Event, je *journeyEnricher)
	}{
		{
			"stderr",
			&journeyEnricher{},
			&SynthEvent{
				Type: "stderr",
				Payload: common.MapStr{
					"message": "Error from synthetics",
				},
				PackageVersion: "1.0.0",
			},
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				v := lookslike.MustCompile(common.MapStr{
					"synthetics": common.MapStr{
						"payload": common.MapStr{
							"message": "Error from synthetics",
						},
						"type":            "stderr",
						"package_version": "1.0.0",
						"index":           0,
					},
				})
				testslike.Test(t, v, e.Fields)
			},
		},
		{
			"stdout",
			&journeyEnricher{},
			&SynthEvent{
				Type: "stdout",
				Payload: common.MapStr{
					"message": "debug output",
				},
				PackageVersion: "1.0.0",
			},
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				v := lookslike.MustCompile(common.MapStr{
					"synthetics": common.MapStr{
						"payload": common.MapStr{
							"message": "debug output",
						},
						"type":            "stdout",
						"package_version": "1.0.0",
						"index":           0,
					},
				})
				testslike.Test(t, lookslike.Strict(v), e.Fields)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &beat.Event{}
			//nolint:errcheck // There are no new changes to this line but
			// linter has been activated in the meantime. We'll cleanup separately.
			tt.je.enrichSynthEvent(e, tt.se)
			tt.check(t, e, tt.je)
		})
	}
}

func TestEnrichSynthEvent(t *testing.T) {
	tests := []struct {
		name    string
		je      *journeyEnricher
		se      *SynthEvent
		wantErr bool
		check   func(t *testing.T, e *beat.Event, je *journeyEnricher)
	}{
		{
			"cmd/status - with error",
			&journeyEnricher{},
			&SynthEvent{
				Type:  CmdStatus,
				Error: &SynthError{Name: "cmdexit", Message: "cmd err msg"},
			},
			true,
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				v := lookslike.MustCompile(common.MapStr{
					"summary": map[string]int{
						"up":   0,
						"down": 1,
					},
				})
				testslike.Test(t, v, e.Fields)
			},
		},
		{
			// If a journey did not emit `journey/end` but exited without
			// errors, we consider the journey to be up.
			"cmd/status - without error",
			&journeyEnricher{},
			&SynthEvent{
				Type:  CmdStatus,
				Error: nil,
			},
			true,
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				v := lookslike.MustCompile(common.MapStr{
					"summary": map[string]int{
						"up":   1,
						"down": 0,
					},
				})
				testslike.Test(t, v, e.Fields)
			},
		},
		{
			"journey/end",
			&journeyEnricher{},
			&SynthEvent{Type: "journey/end"},
			false,
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				v := lookslike.MustCompile(common.MapStr{
					"summary": map[string]int{
						"up":   1,
						"down": 0,
					},
				})
				testslike.Test(t, v, e.Fields)
			},
		},
		{
			"step/end",
			&journeyEnricher{},
			&SynthEvent{Type: "step/end"},
			false,
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				require.Equal(t, 1, je.stepCount)
			},
		},
		{
			"step/screenshot",
			&journeyEnricher{},
			&SynthEvent{Type: "step/screenshot"},
			false,
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				require.Equal(t, "browser.screenshot", e.Meta[add_data_stream.FieldMetaCustomDataset])
			},
		},
		{
			"step/screenshot_ref",
			&journeyEnricher{},
			&SynthEvent{Type: "step/screenshot_ref"},
			false,
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				require.Equal(t, "browser.screenshot", e.Meta[add_data_stream.FieldMetaCustomDataset])
			},
		},
		{
			"step/screenshot_block",
			&journeyEnricher{},
			&SynthEvent{Type: "screenshot/block", Id: "my_id"},
			false,
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				require.Equal(t, "my_id", e.Meta["_id"])
				require.Equal(t, events.OpTypeCreate, e.Meta[events.FieldMetaOpType])
				require.Equal(t, "browser.screenshot", e.Meta[add_data_stream.FieldMetaCustomDataset])
			},
		},
		{
			"journey/network_info",
			&journeyEnricher{},
			&SynthEvent{Type: "journey/network_info"},
			false,
			func(t *testing.T, e *beat.Event, je *journeyEnricher) {
				require.Equal(t, "browser.network", e.Meta[add_data_stream.FieldMetaCustomDataset])
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &beat.Event{}
			if err := tt.je.enrichSynthEvent(e, tt.se); (err == nil && tt.wantErr) || (err != nil && !tt.wantErr) {
				t.Errorf("journeyEnricher.enrichSynthEvent() error = %v, wantErr %v", err, tt.wantErr)
			}
			tt.check(t, e, tt.je)
		})
	}
}

func TestNoSummaryOnAfterHook(t *testing.T) {
	journey := &Journey{
		Name: "A journey that fails after completing",
		Id:   "my-bad-after-all-hook",
	}
	journeyStart := &SynthEvent{
		Type:                 "journey/start",
		TimestampEpochMicros: 1000,
		PackageVersion:       "1.0.0",
		Journey:              journey,
		Payload:              common.MapStr{},
	}
	syntherr := &SynthError{
		Message: "my-errmsg",
		Name:    "my-errname",
		Stack:   "my\nerr\nstack",
	}
	journeyEnd := &SynthEvent{
		Type:                 "journey/end",
		TimestampEpochMicros: 2000,
		PackageVersion:       "1.0.0",
		Journey:              journey,
		Payload:              common.MapStr{},
	}
	cmdStatus := &SynthEvent{
		Type:                 CmdStatus,
		Error:                &SynthError{Name: "cmdexit", Message: "cmd err msg"},
		TimestampEpochMicros: 3000,
	}

	badStepUrl := "https://example.com/bad-step"
	synthEvents := []*SynthEvent{
		journeyStart,
		makeStepEvent("step/start", 10, "Step1", 1, "", "", nil),
		makeStepEvent("step/end", 20, "Step1", 1, "failed", badStepUrl, syntherr),
		journeyEnd,
		cmdStatus,
	}

	je := &journeyEnricher{}

	for idx, se := range synthEvents {
		e := &beat.Event{}
		stdFields := StdSuiteFields{IsInline: false}
		t.Run(fmt.Sprintf("event %d", idx), func(t *testing.T) {
			enrichErr := je.enrich(e, se, stdFields)

			if se != nil && se.Type == CmdStatus {
				t.Run("no summary in cmd/status", func(t *testing.T) {
					require.NotContains(t, e.Fields, "summary")
				})
			}

			// Only the journey/end event should get a summary when
			// it's emitted before the cmd/status (when an afterX hook fails).
			if se != nil && se.Type == JourneyEnd {
				require.Equal(t, stepError(syntherr), enrichErr)

				u, _ := url.Parse(badStepUrl)
				t.Run("summary in journey/end", func(t *testing.T) {
					v := lookslike.MustCompile(common.MapStr{
						"synthetics.type":     "heartbeat/summary",
						"url":                 wrappers.URLFields(u),
						"monitor.duration.us": int64(journeyEnd.Timestamp().Sub(journeyStart.Timestamp()) / time.Microsecond),
					})

					testslike.Test(t, v, e.Fields)
				})
			}
		})
	}
}

func TestSummaryWithoutJourneyEnd(t *testing.T) {
	journey := &Journey{
		Name: "A journey that never emits journey/end but exits successfully",
		Id:   "no-journey-end-but-success",
	}
	journeyStart := &SynthEvent{
		Type:                 "journey/start",
		TimestampEpochMicros: 1000,
		PackageVersion:       "1.0.0",
		Journey:              journey,
		Payload:              common.MapStr{},
	}

	cmdStatus := &SynthEvent{
		Type:                 CmdStatus,
		Error:                nil,
		TimestampEpochMicros: 3000,
	}

	url1 := "http://example.net/url1"
	synthEvents := []*SynthEvent{
		journeyStart,
		makeStepEvent("step/end", 20, "Step1", 1, "", url1, nil),
		cmdStatus,
	}

	je := &journeyEnricher{}

	hasCmdStatus := false

	for idx, se := range synthEvents {
		e := &beat.Event{}
		stdFields := StdSuiteFields{IsInline: false}
		t.Run(fmt.Sprintf("event %d", idx), func(t *testing.T) {
			enrichErr := je.enrich(e, se, stdFields)

			if se != nil && se.Type == CmdStatus {
				hasCmdStatus = true
				require.Error(t, enrichErr, "journey did not finish executing, 1 steps ran")

				u, _ := url.Parse(url1)

				v := lookslike.MustCompile(common.MapStr{
					"synthetics.type":     "heartbeat/summary",
					"url":                 wrappers.URLFields(u),
					"monitor.duration.us": int64(cmdStatus.Timestamp().Sub(journeyStart.Timestamp()) / time.Microsecond),
				})

				testslike.Test(t, v, e.Fields)
			}
		})
	}

	require.True(t, hasCmdStatus)
}

func TestCreateSummaryEvent(t *testing.T) {
	baseTime := time.Now()

	defaultLogValidator := func(stepCount int) func(t *testing.T, summary common.MapStr, observed []observer.LoggedEntry) {
		return func(t *testing.T, summary common.MapStr, observed []observer.LoggedEntry) {
			require.Len(t, observed, 1)
			require.Equal(t, "Monitor finished", observed[0].Message)

			durationMs := baseTime.Add(10 * time.Microsecond).Sub(baseTime).Milliseconds()
			expectedMonitor := logger.NewMonitorRunInfo("my-monitor", "browser", durationMs)
			expectedMonitor.Steps = &stepCount
			require.ElementsMatch(t, []zap.Field{
				logp.Any("event", map[string]string{"action": logger.ActionMonitorRun}),
				logp.Any("monitor", &expectedMonitor),
			}, observed[0].Context)
		}
	}

	tests := []struct {
		name         string
		je           *journeyEnricher
		expected     common.MapStr
		wantErr      bool
		logValidator func(t *testing.T, summary common.MapStr, observed []observer.LoggedEntry)
	}{{
		name: "completed without errors",
		je: &journeyEnricher{
			journey:         &Journey{},
			start:           baseTime,
			end:             baseTime.Add(10 * time.Microsecond),
			journeyComplete: true,
			stepCount:       3,
		},
		expected: common.MapStr{
			"monitor.duration.us": int64(10),
			"summary": common.MapStr{
				"down": 0,
				"up":   1,
			},
		},
		wantErr:      false,
		logValidator: defaultLogValidator(3),
	}, {
		name: "completed with error",
		je: &journeyEnricher{
			journey:         &Journey{},
			start:           baseTime,
			end:             baseTime.Add(10 * time.Microsecond),
			journeyComplete: true,
			errorCount:      1,
			firstError:      fmt.Errorf("journey errored"),
		},
		expected: common.MapStr{
			"monitor.duration.us": int64(10),
			"summary": common.MapStr{
				"down": 1,
				"up":   0,
			},
		},
		wantErr:      true,
		logValidator: defaultLogValidator(0),
	}, {
		name: "started, but exited without running steps",
		je: &journeyEnricher{
			journey:         &Journey{},
			start:           baseTime,
			end:             baseTime.Add(10 * time.Microsecond),
			stepCount:       0,
			journeyComplete: false,
		},
		expected: common.MapStr{
			"monitor.duration.us": int64(10),
			"summary": common.MapStr{
				"down": 0,
				"up":   1,
			},
		},
		wantErr:      true,
		logValidator: defaultLogValidator(0),
	}, {
		name: "syntax error - exited without starting",
		je: &journeyEnricher{
			journey:         &Journey{},
			end:             time.Now().Add(10 * time.Microsecond),
			journeyComplete: false,
			errorCount:      1,
		},
		expected: common.MapStr{
			"summary": common.MapStr{
				"down": 1,
				"up":   0,
			},
		},
		logValidator: func(t *testing.T, summary common.MapStr, observed []observer.LoggedEntry) {
			// We don't log run data without duration
			require.Len(t, observed, 1)
			require.Equal(t, "Error gathering information to log event", observed[0].Message)
		},
		wantErr: true,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, observed := observer.New(zapcore.InfoLevel)
			logger.SetLogger(logp.NewLogger("t", zap.WrapCore(func(in zapcore.Core) zapcore.Core {
				return zapcore.NewTee(in, core)
			})))

			monitorField := common.MapStr{"id": "my-monitor", "type": "browser"}

			e := &beat.Event{
				Fields: common.MapStr{"monitor": monitorField},
			}
			err := tt.je.createSummary(e)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			//nolint:errcheck // There are no new changes to this line but
			// linter has been activated in the meantime. We'll cleanup separately.
			common.MergeFields(tt.expected, common.MapStr{
				"monitor":            monitorField,
				"url":                common.MapStr{},
				"event.type":         "heartbeat/summary",
				"synthetics.type":    "heartbeat/summary",
				"synthetics.journey": Journey{},
			}, true)
			testslike.Test(t, lookslike.Strict(lookslike.MustCompile(tt.expected)), e.Fields)

			if tt.logValidator != nil {
				tt.logValidator(t, tt.expected, observed.All())
			}
		})
	}
}
