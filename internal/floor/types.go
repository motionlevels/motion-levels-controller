package floor

const (
	GridWidth             = 16
	GridHeight            = 32
	TileCount             = GridWidth * GridHeight
	RGBByteCount          = TileCount * 3
	PressureByteCount     = (TileCount + 7) / 8
	DefaultControllers    = 1
	DefaultChannels       = 8
	DefaultLEDsPerChannel = 64
)

type RGB struct {
	R byte
	G byte
	B byte
}

var Black = RGB{}
