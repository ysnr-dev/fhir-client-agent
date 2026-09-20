package orcapush

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

func TestTranslate(t *testing.T) {
	const at = "2026-09-20T10:15:00+09:00"
	accept := func(mode string) map[string]any {
		return map[string]any{
			"Patient_Mode": mode, "Patient_ID": "00002",
			"Accept_Date": "2026-09-20", "Accept_Time": "10:16:00", "Accept_Id": "00003",
			"Department_Code": "01", "Physician_Code": "10001",
			"Insurance_Combination_Number": "0001",
		}
	}
	reception := &event.Reception{
		Key: "2026-09-20:00003", Date: "2026-09-20", Time: "10:16:00",
		DepartmentCode: "01", PhysicianCode: "10001", CoverageSetKey: "0001",
	}

	tests := []struct {
		name string
		in   notification
		want event.Event
		ok   bool
	}{
		{
			// 保険・公費の変更に専用のイベントは無く、患者登録の更新として飛んでくる。
			// イベント名は日レセが送ってくる綴り(patient_infomation)。
			name: "患者登録の更新は patient.changed",
			in: notification{UUID: "u-1", Event: "patient_infomation", Time: at,
				Body: map[string]any{"Patient_Mode": "modify", "Patient_ID": "00002"}},
			want: event.Event{ID: "u-1", Type: event.PatientChanged, OccurredAt: at, PatientNumber: "00002"},
			ok:   true,
		},
		{
			name: "患者登録の新規も patient.changed",
			in: notification{UUID: "u-1", Event: "patient_infomation", Time: at,
				Body: map[string]any{"Patient_Mode": "add", "Patient_ID": "00002"}},
			want: event.Event{ID: "u-1", Type: event.PatientChanged, OccurredAt: at, PatientNumber: "00002"},
			ok:   true,
		},
		{
			name: "患者削除は patient.deleted",
			in: notification{UUID: "u-1", Event: "patient_infomation", Time: at,
				Body: map[string]any{"Patient_Mode": "delete", "Patient_ID": "00002"}},
			want: event.Event{ID: "u-1", Type: event.PatientDeleted, OccurredAt: at, PatientNumber: "00002"},
			ok:   true,
		},
		{
			// 日レセ側の綴りが直った版が来ても拾えること。
			name: "綴りが直った patient_information も patient.changed",
			in: notification{UUID: "u-1", Event: "patient_information", Time: at,
				Body: map[string]any{"Patient_Mode": "modify", "Patient_ID": "00002"}},
			want: event.Event{ID: "u-1", Type: event.PatientChanged, OccurredAt: at, PatientNumber: "00002"},
			ok:   true,
		},
		{
			name: "受付は受付の中身を運ぶ",
			in:   notification{UUID: "u-2", Event: "patient_accept", Time: at, Body: accept("add")},
			want: event.Event{ID: "u-2", Type: event.ReceptionCreated, OccurredAt: at, PatientNumber: "00002", Reception: reception},
			ok:   true,
		},
		{
			name: "受付の更新",
			in:   notification{UUID: "u-2", Event: "patient_accept", Time: at, Body: accept("modify")},
			want: event.Event{ID: "u-2", Type: event.ReceptionUpdated, OccurredAt: at, PatientNumber: "00002", Reception: reception},
			ok:   true,
		},
		{
			name: "受付の取消",
			in:   notification{UUID: "u-2", Event: "patient_accept", Time: at, Body: accept("delete")},
			want: event.Event{ID: "u-2", Type: event.ReceptionCanceled, OccurredAt: at, PatientNumber: "00002", Reception: reception},
			ok:   true,
		},
		{
			// 受付の鍵が作れないと冪等に取り込めないので、送らずに捨てる。
			name: "Accept_Id の無い受付は捨てる",
			in: notification{UUID: "u-3", Event: "patient_accept",
				Body: map[string]any{"Patient_Mode": "add", "Patient_ID": "00002", "Accept_Date": "2026-09-20"}},
		},
		{
			name: "カルテが使わないイベントは捨てる(会計)",
			in:   notification{UUID: "u-4", Event: "patient_account", Body: map[string]any{"Patient_Mode": "add"}},
		},
		{
			name: "カルテが使わないイベントは捨てる(帳票)",
			in:   notification{UUID: "u-5", Event: "print001"},
		},
		{
			name: "Patient_Mode が未知なら捨てる",
			in: notification{UUID: "u-6", Event: "patient_information",
				Body: map[string]any{"Patient_Mode": "???", "Patient_ID": "1"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := translate(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// uuid が欠けていても送れるようにする。
func TestTranslateInventsEventID(t *testing.T) {
	got, ok := translate(notification{Event: "patient_information",
		Body: map[string]any{"Patient_Mode": "add", "Patient_ID": "1"}})
	if !ok {
		t.Fatal("should translate")
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(got.ID) {
		t.Errorf("event_id = %q, want a v4 uuid", got.ID)
	}
}
