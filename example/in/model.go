package in

type User struct{}

// @go2proto
type EventSubForm struct {
	ID               string
	Caption          string
	Rank             int32
	Fields           *ArrayOfEventField
	User             User
	PrimitivePointer *int
	SliceInt         []int
}

// @go2proto
type ArrayOfEventField struct {
	EventField []*EventField
}

// @go2proto
type EventField struct {
	ID               string
	Name             string
	FieldType        string
	IsMandatory      bool
	Rank             int32
	Tag              string
	Items            *ArrayOfEventFieldItem
	CustomFieldOrder int32
}

// @go2proto
type ArrayOfEventFieldItem struct {
	EventFieldItem []*EventFieldItem
}

// @go2proto
type EventFieldItemType string

const (
	EventFieldItemTypeText  EventFieldItemType = "text"
	EventFieldItemTypeFloat EventFieldItemType = "float"
)

// @go2proto
type EventFieldItem struct {
	EventFieldItemID string
	Text             string
	Rank             int32
	FloatField1      float32
	FloatField2      float64
	ItemType         EventFieldItemType
}
