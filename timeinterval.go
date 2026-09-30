package dnp3

// TimeAndInterval is an indexed group 50 variation 4 value. Units is the
// DNP3 interval-units code; it does not change the outstation clock.
type TimeAndInterval struct {
	Time     Timestamp
	Interval uint32
	Units    uint8
}
