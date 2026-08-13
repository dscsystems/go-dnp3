// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

// Package dnp3 implements the DNP3 protocol (IEEE Std 1815-2012) for both
// master and outstation roles.
//
// The stack is layered the way the standard is. Each layer below this package
// exposes a core that takes bytes and returns bytes — it performs no I/O,
// starts no goroutines, and reads no clock. Sessions and channels supply those
// concerns from the outside, which is what makes every parser fuzzable and
// every state machine testable against a virtual clock.
//
//	internal/link       framing, CRC-16/DNP, primary and secondary state machines
//	internal/transport  FIR/FIN/SEQ segmentation and reassembly
//	internal/app        application headers, function codes, IIN, qualifiers
//	objects             group/variation codecs
//	master, outstation  session state machines built on the layers above
//	channel             TCP, TLS, UDP, serial and in-process pipe transports
//
// This root package holds the value types the whole stack shares: measurements,
// quality flags, timestamps, commands and their status codes.
package dnp3

// Version is the library version, following semantic versioning.
const Version = "0.1.0-dev"
