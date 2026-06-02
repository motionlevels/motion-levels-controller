package floor

const (
	GridWidth             = 16
	GridHeight            = 32
	DefaultControllers    = 1
	DefaultChannels       = 8
	DefaultLEDsPerChannel = 64
)

type RGB struct {
	R byte `json:"r"`
	G byte `json:"g"`
	B byte `json:"b"`
}

type Tile struct {
	X       int  `json:"x"`
	Y       int  `json:"y"`
	R       byte `json:"r"`
	G       byte `json:"g"`
	B       byte `json:"b"`
	Pressed bool `json:"pressed"`
}

var Black = RGB{0, 0, 0}
