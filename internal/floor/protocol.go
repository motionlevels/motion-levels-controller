package floor

import (
	"fmt"
	"math/rand"
)

var passWordTable = [256]byte{
	35, 63, 187, 69, 107, 178, 92, 76, 39, 69, 205, 37, 223, 255, 165, 231,
	16, 220, 99, 61, 25, 203, 203, 155, 107, 30, 92, 144, 218, 194, 226, 88,
	196, 190, 67, 195, 159, 185, 209, 24, 163, 65, 25, 172, 126, 63, 224, 61,
	160, 80, 125, 91, 239, 144, 25, 141, 183, 204, 171, 188, 255, 162, 104, 225,
	186, 91, 232, 3, 100, 208, 49, 211, 37, 192, 20, 99, 27, 92, 147, 152,
	86, 177, 53, 153, 94, 177, 200, 33, 175, 195, 15, 228, 247, 18, 244, 150,
	165, 229, 212, 96, 84, 200, 168, 191, 38, 112, 171, 116, 121, 186, 147, 203,
	30, 118, 115, 159, 238, 139, 60, 57, 235, 213, 159, 198, 160, 50, 97, 201,
	253, 242, 240, 77, 102, 12, 183, 235, 243, 247, 75, 90, 13, 236, 56, 133,
	150, 128, 138, 190, 140, 13, 213, 18, 7, 117, 255, 45, 69, 214, 179, 50,
	28, 66, 123, 239, 190, 73, 142, 218, 253, 5, 212, 174, 152, 75, 226, 226,
	172, 78, 35, 93, 250, 238, 19, 32, 247, 223, 89, 123, 86, 138, 150, 146,
	214, 192, 93, 152, 156, 211, 67, 51, 195, 165, 66, 10, 10, 31, 1, 198,
	234, 135, 34, 128, 208, 200, 213, 169, 238, 74, 221, 208, 104, 170, 166, 36,
	76, 177, 196, 3, 141, 167, 127, 56, 177, 203, 45, 107, 46, 82, 217, 139,
	168, 45, 198, 6, 43, 11, 57, 88, 182, 84, 189, 29, 35, 143, 138, 171,
}

const maxPacketSize = 1001

type ColorFunc func(controller, channel, position int) RGB

func BuildSyncPacket(r1, r2 byte, channelCount int, subnetMask []byte) []byte {
	packet := make([]byte, 23)
	packet[0] = 0x67
	packet[1] = r1
	packet[2] = r2
	packet[3] = 0x00
	packet[4] = 0x0A
	packet[5] = 0x02

	model := fmt.Sprintf("KX-HC0%d", channelCount)
	copy(packet[6:13], []byte(model))

	channelIndex := map[int]byte{1: 1, 2: 2, 4: 3, 8: 4}
	packet[13] = channelIndex[channelCount]

	if len(subnetMask) >= 4 {
		copy(packet[16:20], subnetMask[:4])
	} else {
		packet[16] = 255
		packet[17] = 255
		packet[18] = 255
		packet[19] = 255
	}
	packet[21] = 0x14
	packet[22] = syncChecksum(packet)
	return packet
}

func BuildFrame(numControllers, channelCount, ledsPerChannel int, color ColorFunc) [][]byte {
	sequence := rand.Intn(0xFFFF)
	builder := newFrameBuilder()
	builder.addStart(sequence)
	colors := make([]RGB, channelCount)

	for controller := 0; controller < numControllers; controller++ {
		builder.addChannelConfig(controller, channelCount, ledsPerChannel)
		for position := 0; position < ledsPerChannel; position++ {
			for channel := 0; channel < channelCount; channel++ {
				colors[channel] = color(controller, channel, position)
			}
			builder.write(controller, colors)
		}
		builder.finishController()
	}

	builder.addEnd(sequence)
	return builder.packets
}

func syncChecksum(packet []byte) byte {
	var sum int
	for _, b := range packet[:22] {
		sum += int(b)
	}
	return passWordTable[sum&0xFF]
}

type frameBuilder struct {
	packets [][]byte
	buffer  []byte
	cursor  int
}

func newFrameBuilder() *frameBuilder {
	return &frameBuilder{
		packets: make([][]byte, 0, 4),
		cursor:  14,
	}
}

func (b *frameBuilder) addStart(sequence int) {
	b.packets = append(b.packets, []byte{
		0x75, randomByte(), randomByte(), 0, 8, 2, 0, 0,
		0x33, 0x44, byte(sequence >> 8), byte(sequence), 0, 0, 0, 14, 0,
	})
}

func (b *frameBuilder) addEnd(sequence int) {
	b.packets = append(b.packets, []byte{
		0x75, randomByte(), randomByte(), 0, 8, 2, 0, 0,
		0x55, 0x66, byte(sequence >> 8), byte(sequence), 0, 0, 0, 14, 0,
	})
}

func (b *frameBuilder) addChannelConfig(controller, channelCount, ledsPerChannel int) {
	payloadSize := channelCount * 2
	packet := make([]byte, 17+payloadSize)
	packet[0] = 0x75
	packet[1] = randomByte()
	packet[2] = randomByte()
	packet[3] = byte((len(packet) - 9) >> 8)
	packet[4] = byte(len(packet) - 9)
	packet[5] = 2
	packet[6] = byte(controller)
	packet[8] = 0x88
	packet[9] = 0x77
	packet[10] = 0xFF
	packet[11] = 0xF0
	packet[12] = byte(payloadSize >> 8)
	packet[13] = byte(payloadSize)

	for channel := 0; channel < channelCount; channel++ {
		packet[14+channel*2] = byte(ledsPerChannel >> 8)
		packet[15+channel*2] = byte(ledsPerChannel)
	}
	packet[14+payloadSize] = byte((len(packet) - 3) >> 8)
	packet[15+payloadSize] = byte(len(packet) - 3)

	b.packets = append(b.packets, packet)
}

func (b *frameBuilder) write(controller int, colors []RGB) {
	width := len(colors)
	if b.cursor+3*width > 998 {
		b.finishController()
	}
	if b.buffer == nil {
		b.buffer = make([]byte, maxPacketSize)
		b.buffer[0] = 0x75
		b.buffer[1] = randomByte()
		b.buffer[2] = randomByte()
		b.buffer[5] = 2
		b.buffer[7] = byte(controller)
		b.buffer[8] = 0x88
		b.buffer[9] = 0x77
		b.packets = append(b.packets, b.buffer)
	}

	base := b.cursor
	for index, color := range colors {
		b.buffer[base+index] = color.G
		b.buffer[base+width+index] = color.R
		b.buffer[base+2*width+index] = color.B
	}
	b.cursor = base + 3*width
}

func (b *frameBuilder) finishController() {
	if b.buffer == nil {
		return
	}

	last := b.packets[len(b.packets)-1]
	trimmed := make([]byte, b.cursor+3)
	copy(trimmed, last[:b.cursor])
	b.packets[len(b.packets)-1] = trimmed

	rgbSequence := 0
	for _, packet := range b.packets {
		if len(packet) < 14 || packet[0] != 0x75 || packet[8] != 0x88 || packet[9] != 0x77 || packet[10] == 0xFF {
			continue
		}
		packet[3] = byte((len(packet) - 9) >> 8)
		packet[4] = byte(len(packet) - 9)
		packet[10] = byte(rgbSequence >> 8)
		packet[11] = byte(rgbSequence)
		packet[12] = byte((len(packet) - 17) >> 8)
		packet[13] = byte(len(packet) - 17)
		packet[len(packet)-3] = byte((len(packet) - 3) >> 8)
		packet[len(packet)-2] = byte(len(packet) - 3)
		rgbSequence++
	}

	b.buffer = nil
	b.cursor = 14
}

func randomByte() byte {
	return byte(rand.Intn(127))
}
