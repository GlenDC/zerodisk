package gridapistub

type EnumVdiskListItemStatus string

const (
	EnumVdiskListItemStatusrunning     EnumVdiskListItemStatus = "running"
	EnumVdiskListItemStatushalted      EnumVdiskListItemStatus = "halted"
	EnumVdiskListItemStatusrollingback EnumVdiskListItemStatus = "rollingback"
)