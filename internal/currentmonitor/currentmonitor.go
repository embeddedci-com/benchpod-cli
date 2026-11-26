// Package currentmonitor drives the external INA current monitors the embeddedci SDK knows
// (the kind "current" entries of internal/expansion's chips.json, a snapshot of EXPANSION_CHIPS) through the pod's I2C master: probe by ID,
// configure, read. The SDK (bp.expansion("ina226", ...)) and the MCP server's expansion tool are the
// reference; this is the same register model for `benchpod expansion ina226`.
package currentmonitor

import (
	"fmt"
	"math"

	"github.com/embeddedci-com/benchpod-cli/internal/expansion"
)

// Chip is an INA entry of the SDK's EXPANSION_CHIPS (kind "current").
type Chip = expansion.Chip

// Xfer writes then reads one I2C device (a repeated START between), like the pod's i2c_xfer.
type Xfer func(addr int, write []byte, read int) ([]byte, error)

var (
	ctA    = []float64{140e-6, 204e-6, 332e-6, 588e-6, 1.1e-3, 2.116e-3, 4.156e-3, 8.244e-3}
	ctB    = []float64{50e-6, 84e-6, 150e-6, 280e-6, 540e-6, 1052e-6, 2074e-6, 4120e-6}
	avgs   = []int{1, 4, 16, 64, 128, 256, 512, 1024}
	adc219 = []struct {
		n int
		t float64
	}{{1, 84e-6}, {1, 148e-6}, {1, 276e-6}, {1, 532e-6}, {2, 1.06e-3}, {4, 2.13e-3}, {8, 4.26e-3},
		{16, 8.51e-3}, {32, 17.02e-3}, {64, 34.05e-3}, {128, 68.10e-3}}
)

func nearest(opts []float64, want float64) int {
	best := 0
	for i := range opts {
		if math.Abs(opts[i]-want) < math.Abs(opts[best]-want) {
			best = i
		}
	}
	return best
}

func avgIndex(n int) (int, error) {
	for i, a := range avgs {
		if a == n {
			return i, nil
		}
	}
	return 0, fmt.Errorf("averaging is one of 1, 4, 16, 64, 128, 256, 512 or 1024, got %d", n)
}

// Monitor is one chip (INA3221: one channel of it) on an I2C bus.
type Monitor struct {
	Chip     Chip
	Addr     int
	Channel  int
	ShuntOhm float64
	xfer     Xfer

	avg, ct int // averaging count, conversion time index
	adc     int // INA219 ADC field index into adc219
	pga     int // INA219 PGA index
	fine    bool
}

// New returns a monitor with the SDK's defaults: 16 averages, 1.1 ms (INA219: 16 samples).
func New(c Chip, addr, channel int, shuntOhm float64, xfer Xfer) *Monitor {
	return &Monitor{Chip: c, Addr: addr, Channel: channel, ShuntOhm: shuntOhm, xfer: xfer,
		avg: 16, ct: 4, adc: 7, pga: 3}
}

func (m *Monitor) r16(reg int) (int, error) {
	b, err := m.xfer(m.Addr, []byte{byte(reg)}, 2)
	if err != nil {
		return 0, err
	}
	if len(b) != 2 {
		return 0, fmt.Errorf("0x%02x answered %d bytes, not 2", m.Addr, len(b))
	}
	return int(b[0])<<8 | int(b[1]), nil
}

func (m *Monitor) w16(reg, v int) error {
	_, err := m.xfer(m.Addr, []byte{byte(reg), byte(v >> 8), byte(v)}, 0)
	return err
}

func (m *Monitor) block(reg, n int) ([]byte, error) {
	b, err := m.xfer(m.Addr, []byte{byte(reg)}, n)
	if err == nil && len(b) != n {
		err = fmt.Errorf("0x%02x answered %d bytes, not %d", m.Addr, len(b), n)
	}
	return b, err
}

// Probe checks the chip's manufacturer and die ID registers (the INA219 has none).
func (m *Monitor) Probe() error {
	for _, id := range m.Chip.IDs {
		got, err := m.r16(id[0])
		if err != nil {
			return err
		}
		if got&id[1] != id[2] {
			return fmt.Errorf("0x%02x answers, but register 0x%02x reads 0x%04x, not the %s's 0x%04x: another chip, or a different INA type",
				m.Addr, id[0], got, m.Chip.Name, id[2])
		}
	}
	return nil
}

func (m *Monitor) configA() int {
	base := map[string]int{"ina226": 0x4000, "ina260": 0x6000, "ina3221": 0x7000}[m.Chip.Name]
	ai, _ := avgIndex(m.avg)
	return base | ai<<9 | m.ct<<6 | m.ct<<3 | 0b111
}

// Configure resets the chip (when reset) and sets averaging (0: keep 16), the conversion time in
// seconds (0: keep) and, for the INA219, INA228 and INA238, the narrowest range covering
// maxShuntV volts (0: keep the default).
func (m *Monitor) Configure(reset bool, averaging int, convTime, maxShuntV float64) error {
	name := m.Chip.Name
	if maxShuntV > 0 && m.Chip.ShuntMax > 0 && maxShuntV > m.Chip.ShuntMax {
		return fmt.Errorf("%.1f mV across the shunt: the %s takes at most %.2f mV (use a smaller shunt)",
			maxShuntV*1000, name, m.Chip.ShuntMax*1000)
	}
	if reset {
		if err := m.w16(0x00, 0x8000); err != nil {
			return err
		}
	}
	switch name {
	case "ina219":
		if averaging != 0 || convTime != 0 {
			n := averaging
			if n == 0 {
				n = 1
			}
			if n == 1 {
				want := convTime
				if want == 0 {
					want = 532e-6
				}
				ts := make([]float64, 4)
				for i := range ts {
					ts[i] = adc219[i].t
				}
				m.adc = nearest(ts, want)
			} else {
				idx := -1
				for i, a := range adc219 {
					if a.n == n {
						idx = i
					}
				}
				if idx < 0 {
					return fmt.Errorf("averaging for the ina219 is one of 1, 2, 4, 8, 16, 32, 64, 128, got %d", n)
				}
				m.adc = idx
			}
		}
		if maxShuntV > 0 {
			m.pga = 3
			for i, v := range []float64{40e-3, 80e-3, 160e-3, 320e-3} {
				if maxShuntV <= v {
					m.pga = i
					break
				}
			}
		}
		adc := m.adc
		if adc >= 4 {
			adc = 0b1000 | (adc - 3)
		}
		return m.w16(0x00, 1<<13|m.pga<<11|adc<<7|adc<<3|0b111)
	case "ina228", "ina238":
		if averaging != 0 {
			if _, err := avgIndex(averaging); err != nil {
				return err
			}
			m.avg = averaging
		}
		if convTime != 0 {
			m.ct = nearest(ctB, convTime)
		}
		if maxShuntV > 0 {
			m.fine = maxShuntV <= 40.96e-3
		}
		cfg := 0
		if m.fine {
			cfg = 0x10
		}
		if err := m.w16(0x00, cfg); err != nil {
			return err
		}
		ai, _ := avgIndex(m.avg)
		return m.w16(0x01, 0xB<<12|m.ct<<9|m.ct<<6|0b101<<3|ai)
	}
	if averaging != 0 {
		if _, err := avgIndex(averaging); err != nil {
			return err
		}
		m.avg = averaging
	}
	if convTime != 0 {
		m.ct = nearest(ctA, convTime)
	}
	return m.w16(0x00, m.configA())
}

// Cycle is the seconds the chip takes for one new result.
func (m *Monitor) Cycle() float64 {
	switch m.Chip.Name {
	case "ina219":
		return 2 * adc219[m.adc].t
	case "ina228", "ina238":
		return 2 * ctB[m.ct] * float64(m.avg)
	case "ina3221":
		return 2 * ctA[m.ct] * float64(m.avg) * 3
	}
	return 2 * ctA[m.ct] * float64(m.avg)
}

// Reading is one reading: volts, amps, watts.
type Reading struct {
	BusVoltage   float64 `json:"bus_voltage_v"`
	ShuntVoltage float64 `json:"shunt_voltage_v"`
	Current      float64 `json:"current_a"`
	Power        float64 `json:"power_w"`
	Overflow     bool    `json:"overflow"`
}

func s(v, bits int) int {
	if v >= 1<<(bits-1) {
		return v - 1<<bits
	}
	return v
}

func be(b []byte) int {
	v := 0
	for _, x := range b {
		v = v<<8 | int(x)
	}
	return v
}

// Read takes one reading of the latest finished conversion.
func (m *Monitor) Read() (Reading, error) {
	var shunt, bus float64
	var over bool
	switch m.Chip.Name {
	case "ina226":
		b, err := m.block(0x01, 4)
		if err != nil {
			return Reading{}, err
		}
		shunt, bus = float64(s(be(b[:2]), 16))*2.5e-6, float64(be(b[2:]))*1.25e-3
		over = math.Abs(shunt) >= 81.9e-3
	case "ina260":
		b, err := m.block(0x01, 4)
		if err != nil {
			return Reading{}, err
		}
		shunt = float64(s(be(b[:2]), 16)) * 1.25e-3 * m.Chip.InternalOhm
		bus = float64(be(b[2:])) * 1.25e-3
	case "ina3221":
		b, err := m.block(1+2*(m.Channel-1), 4)
		if err != nil {
			return Reading{}, err
		}
		shunt, bus = float64(s(be(b[:2]), 16)>>3)*40e-6, float64(s(be(b[2:]), 16)>>3)*8e-3
		over = math.Abs(shunt) >= 163.8e-3
	case "ina219":
		b, err := m.block(0x01, 4)
		if err != nil {
			return Reading{}, err
		}
		reg := be(b[2:])
		shunt, bus, over = float64(s(be(b[:2]), 16))*10e-6, float64(reg>>3)*4e-3, reg&1 == 1
	case "ina228":
		b, err := m.block(0x04, 6)
		if err != nil {
			return Reading{}, err
		}
		lsb, lim := 312.5e-9, 163.8e-3
		if m.fine {
			lsb, lim = 78.125e-9, 40.9e-3
		}
		shunt, bus = float64(s(be(b[:3]), 24)>>4)*lsb, float64(be(b[3:])>>4)*195.3125e-6
		over = math.Abs(shunt) >= lim
	case "ina238":
		b, err := m.block(0x04, 4)
		if err != nil {
			return Reading{}, err
		}
		lsb, lim := 5e-6, 163.8e-3
		if m.fine {
			lsb, lim = 1.25e-6, 40.9e-3
		}
		shunt, bus = float64(s(be(b[:2]), 16))*lsb, float64(be(b[2:]))*3.125e-3
		over = math.Abs(shunt) >= lim
	default:
		return Reading{}, fmt.Errorf("no driver for %s", m.Chip.Name)
	}
	cur := shunt / m.ShuntOhm
	return Reading{BusVoltage: bus, ShuntVoltage: shunt, Current: cur, Power: bus * cur, Overflow: over}, nil
}
