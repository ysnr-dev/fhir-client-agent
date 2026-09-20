// Package event はカルテへ送る中立なイベント。
//
// ここが連携先の言葉とカルテの言葉の境目。カルテは連携先の製品もイベント形式も
// 知らないので、アダプタは必ずこの形に直してから渡す。
package event

// 種別。カルテ側(Integrations::ReceiptComputer::EventHandler::TYPES)と揃える。
const (
	PatientChanged    = "patient.changed"
	PatientDeleted    = "patient.deleted"
	ReceptionCreated  = "reception.created"
	ReceptionUpdated  = "reception.updated"
	ReceptionCanceled = "reception.canceled"
)

// Event は 1 件の通知。中身は運ばない(何が変わったかはカルテが連携先から取り直す)。
type Event struct {
	ID            string     `json:"event_id"`
	Type          string     `json:"type"`
	OccurredAt    string     `json:"occurred_at,omitempty"`
	PatientNumber string     `json:"patient_number"`
	Reception     *Reception `json:"reception,omitempty"`
}

// Reception は受付。Key は連携先が採番した値から作る冪等キー。
type Reception struct {
	Key            string `json:"key"`
	Date           string `json:"date,omitempty"`
	Time           string `json:"time,omitempty"`
	DepartmentCode string `json:"department_code,omitempty"`
	PhysicianCode  string `json:"physician_code,omitempty"`
	CoverageSetKey string `json:"coverage_set_key,omitempty"`
}
