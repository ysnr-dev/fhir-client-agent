package orcapush

import (
	"crypto/rand"
	"fmt"

	"github.com/ysnr-dev/fhir-client-agent/internal/event"
)

// notification は日レセの PUSH 通知 1 件(command=event の data)。
type notification struct {
	UUID  string         `json:"uuid"`
	Event string         `json:"event"`
	Time  string         `json:"time"`
	Body  map[string]any `json:"body"`
}

// 取り扱う日レセのイベント。これ以外(会計・入退院・帳票など)は捨てる。
//
// 患者情報のイベント名は日レセ側の綴りが patient_infomation(r が無い)。
// 直った版が来ても拾えるように、正しい綴りも受ける。
const (
	eventPatientInformation      = "patient_infomation"
	eventPatientInformationFixed = "patient_information"
	eventPatientAccept           = "patient_accept"
)

// translate は日レセの通知を中立イベントにする。対象外なら ok=false。
//
// 通知は「何が変わったか」を運ばない(患者番号と日時だけ)。中身はカルテが
// 日レセの API で取り直すので、ここでは種別と鍵だけを渡す。
func translate(n notification) (event.Event, bool) {
	mode := str(n.Body, "Patient_Mode")
	if mode != "add" && mode != "modify" && mode != "delete" {
		return event.Event{}, false
	}

	e := event.Event{
		ID:            n.UUID,
		OccurredAt:    n.Time,
		PatientNumber: str(n.Body, "Patient_ID"),
	}
	// uuid が欠けていても送れるようにする。再送で重複しても、取り込み側は
	// identifier で上書きするので二重にはならない。
	if e.ID == "" {
		e.ID = newUUID()
	}

	switch n.Event {
	case eventPatientInformation, eventPatientInformationFixed:
		// 保険・公費の変更も患者登録の一部として通知される。専用のイベントは無いので、
		// 更新はすべて「患者が変わった」として送る。
		e.Type = event.PatientChanged
		if mode == "delete" {
			e.Type = event.PatientDeleted
		}
		return e, true

	case eventPatientAccept:
		date, id := str(n.Body, "Accept_Date"), str(n.Body, "Accept_Id")
		// 鍵が揃わないと冪等に取り込めないので、送らずに捨てる。
		if date == "" || id == "" {
			return event.Event{}, false
		}
		e.Type = map[string]string{
			"add": event.ReceptionCreated, "modify": event.ReceptionUpdated, "delete": event.ReceptionCanceled,
		}[mode]
		e.Reception = &event.Reception{
			Key:            date + ":" + id,
			Date:           date,
			Time:           str(n.Body, "Accept_Time"),
			DepartmentCode: str(n.Body, "Department_Code"),
			PhysicianCode:  str(n.Body, "Physician_Code"),
			CoverageSetKey: str(n.Body, "Insurance_Combination_Number"),
		}
		return e, true
	}
	return event.Event{}, false
}

func str(body map[string]any, key string) string {
	if v, ok := body[key].(string); ok {
		return v
	}
	return ""
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
