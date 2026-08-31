package amd64

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/samyfodil/wazy/internal/engine/native/backend"
	"github.com/samyfodil/wazy/internal/engine/native/backend/regalloc"
	"github.com/samyfodil/wazy/internal/engine/native/nativeapi"
	"github.com/samyfodil/wazy/internal/engine/native/ssa"
)

func (i *instruction) encode(c backend.Compiler) (needsLabelResolution bool) {
	switch kind := i.kind; kind {
	case nop0, sourceOffsetInfo, defineUninitializedReg, fcvtToSintSequence, fcvtToUintSequence, nopUseReg:
	case ret:
		encodeRet(c)
	case imm:
		dst := regEncodings[i.op2.reg().RealReg()]
		con := i.u1
		if i.b1 { // 64 bit.
			if con <= 0xffffffff {
				// C19: the value fits in 32 bits, so `mov r32, imm32` (0xb8),
				// which zero-extends its result to the full 64-bit register on
				// x86-64, materializes the identical 64-bit value in 5-6 bytes
				// -- shorter than both the REX.W sign-extended `mov r/m64, imm32`
				// (7 bytes, used below for large sign-extending consts like -1)
				// and `movabsq` (10 bytes). Covers all of [0, 2^32), including the
				// [2^31, 2^32) range that does NOT sign-extend and previously
				// forced a 10-byte movabsq. Identical encoding to the 32-bit
				// (!i.b1) arm below.
				if dst.rexBit() > 0 {
					c.EmitByte(rexEncodingDefault | 0x1)
				}
				c.EmitByte(0xb8 | dst.encoding())
				c.Emit4Bytes(uint32(con))
			} else if lower32willSignExtendTo64(con) {
				// Sign extend mov(imm32) -- a 64-bit value whose low 32 bits
				// sign-extend to it (e.g. -1 = 0xffffffffffffffff), so imm32
				// suffices with REX.W.
				encodeRegReg(c,
					legacyPrefixesNone,
					0xc7, 1,
					0,
					dst,
					rexInfo(0).setW(),
				)
				c.Emit4Bytes(uint32(con))
			} else {
				c.EmitByte(rexEncodingW | dst.rexBit())
				c.EmitByte(0xb8 | dst.encoding())
				c.Emit8Bytes(con)
			}
		} else {
			if dst.rexBit() > 0 {
				c.EmitByte(rexEncodingDefault | 0x1)
			}
			c.EmitByte(0xb8 | dst.encoding())
			c.Emit4Bytes(uint32(con))
		}

	case aluRmiR:
		var rex rexInfo
		if i.b1 {
			rex = rex.setW()
		} else {
			rex = rex.clearW()
		}

		dst := regEncodings[i.op2.reg().RealReg()]

		aluOp := aluRmiROpcode(i.u1)
		if aluOp == aluRmiROpcodeMul {
			op1 := i.op1
			const regMemOpc, regMemOpcNum = 0x0FAF, 2
			switch op1.kind {
			case operandKindReg:
				src := regEncodings[op1.reg().RealReg()]
				encodeRegReg(c, legacyPrefixesNone, regMemOpc, regMemOpcNum, dst, src, rex)
			case operandKindMem:
				m := i.op1.addressMode()
				encodeRegMem(c, legacyPrefixesNone, regMemOpc, regMemOpcNum, dst, m, rex)
			case operandKindImm32:
				imm8 := lower8willSignExtendTo32(op1.imm32())
				var opc uint32
				if imm8 {
					opc = 0x6b
				} else {
					opc = 0x69
				}
				encodeRegReg(c, legacyPrefixesNone, opc, 1, dst, dst, rex)
				if imm8 {
					c.EmitByte(byte(op1.imm32()))
				} else {
					c.Emit4Bytes(op1.imm32())
				}
			default:
				panic("BUG: invalid operand kind")
			}
		} else {
			const opcodeNum = 1
			var opcR, opcM, subOpcImm uint32
			switch aluOp {
			case aluRmiROpcodeAdd:
				opcR, opcM, subOpcImm = 0x01, 0x03, 0x0
			case aluRmiROpcodeSub:
				opcR, opcM, subOpcImm = 0x29, 0x2b, 0x5
			case aluRmiROpcodeAnd:
				opcR, opcM, subOpcImm = 0x21, 0x23, 0x4
			case aluRmiROpcodeOr:
				opcR, opcM, subOpcImm = 0x09, 0x0b, 0x1
			case aluRmiROpcodeXor:
				opcR, opcM, subOpcImm = 0x31, 0x33, 0x6
			default:
				panic("BUG: invalid aluRmiROpcode")
			}

			op1 := i.op1
			switch op1.kind {
			case operandKindReg:
				src := regEncodings[op1.reg().RealReg()]
				encodeRegReg(c, legacyPrefixesNone, opcR, opcodeNum, src, dst, rex)
			case operandKindMem:
				m := i.op1.addressMode()
				encodeRegMem(c, legacyPrefixesNone, opcM, opcodeNum, dst, m, rex)
			case operandKindImm32:
				imm8 := lower8willSignExtendTo32(op1.imm32())
				var opc uint32
				if imm8 {
					opc = 0x83
				} else {
					opc = 0x81
				}
				encodeRegReg(c, legacyPrefixesNone, opc, opcodeNum, regEnc(subOpcImm), dst, rex)
				if imm8 {
					c.EmitByte(byte(op1.imm32()))
				} else {
					c.Emit4Bytes(op1.imm32())
				}
			default:
				panic("BUG: invalid operand kind")
			}
		}

	case movRR:
		src := regEncodings[i.op1.reg().RealReg()]
		dst := regEncodings[i.op2.reg().RealReg()]
		var rex rexInfo
		if i.b1 {
			rex = rex.setW()
		} else {
			rex = rex.clearW()
		}
		encodeRegReg(c, legacyPrefixesNone, 0x89, 1, src, dst, rex)

	case xmmRmR, blendvpd:
		op := sseOpcode(i.u1)
		var legPrex legacyPrefixes
		var opcode uint32
		var opcodeNum uint32
		switch op {
		case sseOpcodeAddps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F58, 2
		case sseOpcodeAddpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F58, 2
		case sseOpcodeAddss:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F58, 2
		case sseOpcodeAddsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F58, 2
		case sseOpcodeAndps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F54, 2
		case sseOpcodeAndpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F54, 2
		case sseOpcodeAndnps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F55, 2
		case sseOpcodeAndnpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F55, 2
		case sseOpcodeBlendvps:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3814, 3
		case sseOpcodeBlendvpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3815, 3
		case sseOpcodeDivps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F5E, 2
		case sseOpcodeDivpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F5E, 2
		case sseOpcodeDivss:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F5E, 2
		case sseOpcodeDivsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F5E, 2
		case sseOpcodeMaxps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F5F, 2
		case sseOpcodeMaxpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F5F, 2
		case sseOpcodeMaxss:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F5F, 2
		case sseOpcodeMaxsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F5F, 2
		case sseOpcodeMinps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F5D, 2
		case sseOpcodeMinpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F5D, 2
		case sseOpcodeMinss:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F5D, 2
		case sseOpcodeMinsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F5D, 2
		case sseOpcodeMovlhps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F16, 2
		case sseOpcodeMovsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F10, 2
		case sseOpcodeMulps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F59, 2
		case sseOpcodeMulpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F59, 2
		case sseOpcodeMulss:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F59, 2
		case sseOpcodeMulsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F59, 2
		case sseOpcodeOrpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F56, 2
		case sseOpcodeOrps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F56, 2
		case sseOpcodePackssdw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F6B, 2
		case sseOpcodePacksswb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F63, 2
		case sseOpcodePackusdw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F382B, 3
		case sseOpcodePackuswb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F67, 2
		case sseOpcodePaddb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FFC, 2
		case sseOpcodePaddd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FFE, 2
		case sseOpcodePaddq:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FD4, 2
		case sseOpcodePaddw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FFD, 2
		case sseOpcodePaddsb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FEC, 2
		case sseOpcodePaddsw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FED, 2
		case sseOpcodePaddusb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FDC, 2
		case sseOpcodePaddusw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FDD, 2
		case sseOpcodePand:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FDB, 2
		case sseOpcodePandn:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FDF, 2
		case sseOpcodePavgb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FE0, 2
		case sseOpcodePavgw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FE3, 2
		case sseOpcodePcmpeqb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F74, 2
		case sseOpcodePcmpeqw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F75, 2
		case sseOpcodePcmpeqd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F76, 2
		case sseOpcodePcmpeqq:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3829, 3
		case sseOpcodePcmpgtb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F64, 2
		case sseOpcodePcmpgtw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F65, 2
		case sseOpcodePcmpgtd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F66, 2
		case sseOpcodePcmpgtq:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3837, 3
		case sseOpcodePmaddwd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FF5, 2
		case sseOpcodePmaxsb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F383C, 3
		case sseOpcodePmaxsw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FEE, 2
		case sseOpcodePmaxsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F383D, 3
		case sseOpcodePmaxub:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FDE, 2
		case sseOpcodePmaxuw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F383E, 3
		case sseOpcodePmaxud:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F383F, 3
		case sseOpcodePminsb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3838, 3
		case sseOpcodePminsw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FEA, 2
		case sseOpcodePminsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3839, 3
		case sseOpcodePminub:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FDA, 2
		case sseOpcodePminuw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F383A, 3
		case sseOpcodePminud:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F383B, 3
		case sseOpcodePmulld:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3840, 3
		case sseOpcodePmullw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FD5, 2
		case sseOpcodePmuludq:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FF4, 2
		case sseOpcodePor:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FEB, 2
		case sseOpcodePshufb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3800, 3
		case sseOpcodePsubb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FF8, 2
		case sseOpcodePsubd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FFA, 2
		case sseOpcodePsubq:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FFB, 2
		case sseOpcodePsubw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FF9, 2
		case sseOpcodePsubsb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FE8, 2
		case sseOpcodePsubsw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FE9, 2
		case sseOpcodePsubusb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FD8, 2
		case sseOpcodePsubusw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FD9, 2
		case sseOpcodePunpckhbw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F68, 2
		case sseOpcodePunpcklbw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F60, 2
		case sseOpcodePxor:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FEF, 2
		case sseOpcodeSubps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F5C, 2
		case sseOpcodeSubpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F5C, 2
		case sseOpcodeSubss:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F5C, 2
		case sseOpcodeSubsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F5C, 2
		case sseOpcodeXorps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F57, 2
		case sseOpcodeXorpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F57, 2
		case sseOpcodePmulhrsw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F380B, 3
		case sseOpcodeUnpcklps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0F14, 2
		case sseOpcodePmaddubsw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3804, 3
		default:
			if kind == blendvpd {
				legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3815, 3
			} else {
				panic(fmt.Sprintf("Unsupported sseOpcode: %s", op))
			}
		}

		dst := regEncodings[i.op2.reg().RealReg()]

		rex := rexInfo(0).clearW()
		op1 := i.op1
		if op1.kind == operandKindReg {
			src := regEncodings[op1.reg().RealReg()]
			encodeRegReg(c, legPrex, opcode, opcodeNum, dst, src, rex)
		} else if i.op1.kind == operandKindMem {
			m := i.op1.addressMode()
			encodeRegMem(c, legPrex, opcode, opcodeNum, dst, m, rex)
		} else {
			panic("BUG: invalid operand kind")
		}

	case gprToXmm:
		var legPrefix legacyPrefixes
		var opcode uint32
		const opcodeNum = 2
		switch sseOpcode(i.u1) {
		case sseOpcodeMovd, sseOpcodeMovq:
			legPrefix, opcode = legacyPrefixes0x66, 0x0f6e
		case sseOpcodeCvtsi2ss:
			legPrefix, opcode = legacyPrefixes0xF3, 0x0f2a
		case sseOpcodeCvtsi2sd:
			legPrefix, opcode = legacyPrefixes0xF2, 0x0f2a
		default:
			panic(fmt.Sprintf("Unsupported sseOpcode: %s", sseOpcode(i.u1)))
		}

		var rex rexInfo
		if i.b1 {
			rex = rex.setW()
		} else {
			rex = rex.clearW()
		}
		dst := regEncodings[i.op2.reg().RealReg()]

		op1 := i.op1
		if op1.kind == operandKindReg {
			src := regEncodings[op1.reg().RealReg()]
			encodeRegReg(c, legPrefix, opcode, opcodeNum, dst, src, rex)
		} else if i.op1.kind == operandKindMem {
			m := i.op1.addressMode()
			encodeRegMem(c, legPrefix, opcode, opcodeNum, dst, m, rex)
		} else {
			panic("BUG: invalid operand kind")
		}

	case xmmUnaryRmR:
		var prefix legacyPrefixes
		var opcode uint32
		var opcodeNum uint32
		op := sseOpcode(i.u1)
		switch op {
		case sseOpcodeCvtss2sd:
			prefix, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F5A, 2
		case sseOpcodeCvtsd2ss:
			prefix, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F5A, 2
		case sseOpcodeMovaps:
			prefix, opcode, opcodeNum = legacyPrefixesNone, 0x0F28, 2
		case sseOpcodeMovapd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F28, 2
		case sseOpcodeMovdqa:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F6F, 2
		case sseOpcodeMovdqu:
			prefix, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F6F, 2
		case sseOpcodeMovsd:
			prefix, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F10, 2
		case sseOpcodeMovss:
			prefix, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F10, 2
		case sseOpcodeMovups:
			prefix, opcode, opcodeNum = legacyPrefixesNone, 0x0F10, 2
		case sseOpcodeMovupd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F10, 2
		case sseOpcodePabsb:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F381C, 3
		case sseOpcodePabsw:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F381D, 3
		case sseOpcodePabsd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F381E, 3
		case sseOpcodePmovsxbd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3821, 3
		case sseOpcodePmovsxbw:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3820, 3
		case sseOpcodePmovsxbq:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3822, 3
		case sseOpcodePmovsxwd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3823, 3
		case sseOpcodePmovsxwq:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3824, 3
		case sseOpcodePmovsxdq:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3825, 3
		case sseOpcodePmovzxbd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3831, 3
		case sseOpcodePmovzxbw:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3830, 3
		case sseOpcodePmovzxbq:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3832, 3
		case sseOpcodePmovzxwd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3833, 3
		case sseOpcodePmovzxwq:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3834, 3
		case sseOpcodePmovzxdq:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3835, 3
		case sseOpcodeSqrtps:
			prefix, opcode, opcodeNum = legacyPrefixesNone, 0x0F51, 2
		case sseOpcodeSqrtpd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F51, 2
		case sseOpcodeSqrtss:
			prefix, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F51, 2
		case sseOpcodeSqrtsd:
			prefix, opcode, opcodeNum = legacyPrefixes0xF2, 0x0F51, 2
		case sseOpcodeXorps:
			prefix, opcode, opcodeNum = legacyPrefixesNone, 0x0F57, 2
		case sseOpcodeXorpd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F57, 2
		case sseOpcodeCvtdq2ps:
			prefix, opcode, opcodeNum = legacyPrefixesNone, 0x0F5B, 2
		case sseOpcodeCvtdq2pd:
			prefix, opcode, opcodeNum = legacyPrefixes0xF3, 0x0FE6, 2
		case sseOpcodeCvtps2pd:
			prefix, opcode, opcodeNum = legacyPrefixesNone, 0x0F5A, 2
		case sseOpcodeCvtpd2ps:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0F5A, 2
		case sseOpcodeCvttps2dq:
			prefix, opcode, opcodeNum = legacyPrefixes0xF3, 0x0F5B, 2
		case sseOpcodeCvttpd2dq:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0FE6, 2
		default:
			panic(fmt.Sprintf("Unsupported sseOpcode: %s", op))
		}

		dst := regEncodings[i.op2.reg().RealReg()]

		rex := rexInfo(0).clearW()
		op1 := i.op1
		if op1.kind == operandKindReg {
			src := regEncodings[op1.reg().RealReg()]
			encodeRegReg(c, prefix, opcode, opcodeNum, dst, src, rex)
		} else if i.op1.kind == operandKindMem {
			m := i.op1.addressMode()
			needsLabelResolution = encodeRegMem(c, prefix, opcode, opcodeNum, dst, m, rex)
		} else {
			panic("BUG: invalid operand kind")
		}

	case xmmUnaryRmRImm:
		var prefix legacyPrefixes
		var opcode uint32
		var opcodeNum uint32
		op := sseOpcode(i.u1)
		switch op {
		case sseOpcodeRoundps:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0f3a08, 3
		case sseOpcodeRoundss:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0f3a0a, 3
		case sseOpcodeRoundpd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0f3a09, 3
		case sseOpcodeRoundsd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0f3a0b, 3
		}
		rex := rexInfo(0).clearW()
		dst := regEncodings[i.op2.reg().RealReg()]
		op1 := i.op1
		if op1.kind == operandKindReg {
			src := regEncodings[op1.reg().RealReg()]
			encodeRegReg(c, prefix, opcode, opcodeNum, dst, src, rex)
		} else if i.op1.kind == operandKindMem {
			m := i.op1.addressMode()
			encodeRegMem(c, prefix, opcode, opcodeNum, dst, m, rex)
		} else {
			panic("BUG: invalid operand kind")
		}

		c.EmitByte(byte(i.u2))

	case unaryRmR:
		var prefix legacyPrefixes
		var opcode uint32
		var opcodeNum uint32
		op := unaryRmROpcode(i.u1)
		// We assume size is either 32 or 64.
		switch op {
		case unaryRmROpcodeBsr:
			prefix, opcode, opcodeNum = legacyPrefixesNone, 0x0fbd, 2
		case unaryRmROpcodeBsf:
			prefix, opcode, opcodeNum = legacyPrefixesNone, 0x0fbc, 2
		case unaryRmROpcodeLzcnt:
			prefix, opcode, opcodeNum = legacyPrefixes0xF3, 0x0fbd, 2
		case unaryRmROpcodeTzcnt:
			prefix, opcode, opcodeNum = legacyPrefixes0xF3, 0x0fbc, 2
		case unaryRmROpcodePopcnt:
			prefix, opcode, opcodeNum = legacyPrefixes0xF3, 0x0fb8, 2
		default:
			panic(fmt.Sprintf("Unsupported unaryRmROpcode: %s", op))
		}

		dst := regEncodings[i.op2.reg().RealReg()]

		rex := rexInfo(0)
		if i.b1 { // 64 bit.
			rex = rexInfo(0).setW()
		} else {
			rex = rexInfo(0).clearW()
		}
		op1 := i.op1
		if op1.kind == operandKindReg {
			src := regEncodings[op1.reg().RealReg()]
			encodeRegReg(c, prefix, opcode, opcodeNum, dst, src, rex)
		} else if i.op1.kind == operandKindMem {
			m := i.op1.addressMode()
			encodeRegMem(c, prefix, opcode, opcodeNum, dst, m, rex)
		} else {
			panic("BUG: invalid operand kind")
		}

	case not:
		var prefix legacyPrefixes
		src := regEncodings[i.op1.reg().RealReg()]
		rex := rexInfo(0)
		if i.b1 { // 64 bit.
			rex = rexInfo(0).setW()
		} else {
			rex = rexInfo(0).clearW()
		}
		subopcode := uint8(2)
		encodeEncEnc(c, prefix, 0xf7, 1, subopcode, uint8(src), rex)

	case neg:
		var prefix legacyPrefixes
		src := regEncodings[i.op1.reg().RealReg()]
		rex := rexInfo(0)
		if i.b1 { // 64 bit.
			rex = rexInfo(0).setW()
		} else {
			rex = rexInfo(0).clearW()
		}
		subopcode := uint8(3)
		encodeEncEnc(c, prefix, 0xf7, 1, subopcode, uint8(src), rex)

	case div:
		rex := rexInfo(0)
		if i.b1 { // 64 bit.
			rex = rexInfo(0).setW()
		} else {
			rex = rexInfo(0).clearW()
		}
		var subopcode uint8
		if i.u1 != 0 { // Signed.
			subopcode = 7
		} else {
			subopcode = 6
		}

		divisor := i.op1
		if divisor.kind == operandKindReg {
			src := regEncodings[divisor.reg().RealReg()]
			encodeEncEnc(c, legacyPrefixesNone, 0xf7, 1, subopcode, uint8(src), rex)
		} else if divisor.kind == operandKindMem {
			m := divisor.addressMode()
			encodeEncMem(c, legacyPrefixesNone, 0xf7, 1, subopcode, m, rex)
		} else {
			panic("BUG: invalid operand kind")
		}

	case mulHi:
		var prefix legacyPrefixes
		rex := rexInfo(0)
		if i.b1 { // 64 bit.
			rex = rexInfo(0).setW()
		} else {
			rex = rexInfo(0).clearW()
		}

		signed := i.u1 != 0
		var subopcode uint8
		if signed {
			subopcode = 5
		} else {
			subopcode = 4
		}

		// src1 is implicitly rax,
		// dst_lo is implicitly rax,
		// dst_hi is implicitly rdx.
		src2 := i.op1
		if src2.kind == operandKindReg {
			src := regEncodings[src2.reg().RealReg()]
			encodeEncEnc(c, prefix, 0xf7, 1, subopcode, uint8(src), rex)
		} else if src2.kind == operandKindMem {
			m := src2.addressMode()
			encodeEncMem(c, prefix, 0xf7, 1, subopcode, m, rex)
		} else {
			panic("BUG: invalid operand kind")
		}

	case signExtendData:
		if i.b1 { // 64 bit.
			c.EmitByte(0x48)
			c.EmitByte(0x99)
		} else {
			c.EmitByte(0x99)
		}
	case movzxRmR, movsxRmR:
		signed := i.kind == movsxRmR

		ext := extMode(i.u1)
		var opcode uint32
		var opcodeNum uint32
		var rex rexInfo
		switch ext {
		case extModeBL:
			if signed {
				opcode, opcodeNum, rex = 0x0fbe, 2, rex.clearW()
			} else {
				opcode, opcodeNum, rex = 0x0fb6, 2, rex.clearW()
			}
		case extModeBQ:
			if signed {
				opcode, opcodeNum, rex = 0x0fbe, 2, rex.setW()
			} else {
				opcode, opcodeNum, rex = 0x0fb6, 2, rex.setW()
			}
		case extModeWL:
			if signed {
				opcode, opcodeNum, rex = 0x0fbf, 2, rex.clearW()
			} else {
				opcode, opcodeNum, rex = 0x0fb7, 2, rex.clearW()
			}
		case extModeWQ:
			if signed {
				opcode, opcodeNum, rex = 0x0fbf, 2, rex.setW()
			} else {
				opcode, opcodeNum, rex = 0x0fb7, 2, rex.setW()
			}
		case extModeLQ:
			if signed {
				opcode, opcodeNum, rex = 0x63, 1, rex.setW()
			} else {
				opcode, opcodeNum, rex = 0x8b, 1, rex.clearW()
			}
		default:
			panic("BUG: invalid extMode")
		}

		op := i.op1
		dst := regEncodings[i.op2.reg().RealReg()]
		switch op.kind {
		case operandKindReg:
			src := regEncodings[op.reg().RealReg()]
			if ext == extModeBL || ext == extModeBQ {
				// Some destinations must be encoded with REX.R = 1.
				if e := src.encoding(); e >= 4 && e <= 7 {
					rex = rex.always()
				}
			}
			encodeRegReg(c, legacyPrefixesNone, opcode, opcodeNum, dst, src, rex)
		case operandKindMem:
			m := op.addressMode()
			encodeRegMem(c, legacyPrefixesNone, opcode, opcodeNum, dst, m, rex)
		default:
			panic("BUG: invalid operand kind")
		}

	case mov64MR:
		m := i.op1.addressMode()
		encodeLoad64(c, m, i.op2.reg().RealReg())

	case lea:
		dst := regEncodings[i.op2.reg().RealReg()]
		rex := rexInfo(0)
		if i.b1 { // 64-bit; the 32-bit form drops REX.W and zero-extends into the full register.
			rex = rex.setW()
		}
		const opcode, opcodeNum = 0x8d, 1
		switch i.op1.kind {
		case operandKindMem:
			a := i.op1.addressMode()
			encodeRegMem(c, legacyPrefixesNone, opcode, opcodeNum, dst, a, rex)
		case operandKindLabel:
			// Only the RIP-relative form carries a displacement to patch later.
			needsLabelResolution = true
			rex.encode(c, regRexBit(byte(dst)), 0)
			c.EmitByte(byte((opcode) & 0xff))

			// Indicate "LEAQ [RIP + 32bit displacement].
			// https://wiki.osdev.org/X86-64_Instruction_Encoding#32.2F64-bit_addressing
			c.EmitByte(encodeModRM(0b00, dst.encoding(), 0b101))

			// This will be resolved later, so we just emit a placeholder (0xffffffff for testing).
			c.Emit4Bytes(0xffffffff)
		default:
			panic("BUG: invalid operand kind")
		}

	case movRM:
		m := i.op2.addressMode()
		src := regEncodings[i.op1.reg().RealReg()]

		var rex rexInfo
		switch i.u1 {
		case 1:
			if e := src.encoding(); e >= 4 && e <= 7 {
				rex = rex.always()
			}
			encodeRegMem(c, legacyPrefixesNone, 0x88, 1, src, m, rex.clearW())
		case 2:
			encodeRegMem(c, legacyPrefixes0x66, 0x89, 1, src, m, rex.clearW())
		case 4:
			encodeRegMem(c, legacyPrefixesNone, 0x89, 1, src, m, rex.clearW())
		case 8:
			encodeRegMem(c, legacyPrefixesNone, 0x89, 1, src, m, rex.setW())
		default:
			panic(fmt.Sprintf("BUG: invalid size %d: %s", i.u1, i.String()))
		}

	case shiftR:
		src := regEncodings[i.op2.reg().RealReg()]
		amount := i.op1

		var opcode uint32
		var prefix legacyPrefixes
		rex := rexInfo(0)
		if i.b1 { // 64 bit.
			rex = rexInfo(0).setW()
		} else {
			rex = rexInfo(0).clearW()
		}

		switch amount.kind {
		case operandKindReg:
			if amount.reg() != rcxVReg {
				panic("BUG: invalid reg operand: must be rcx")
			}
			opcode, prefix = 0xd3, legacyPrefixesNone
			encodeEncEnc(c, prefix, opcode, 1, uint8(i.u1), uint8(src), rex)
		case operandKindImm32:
			opcode, prefix = 0xc1, legacyPrefixesNone
			encodeEncEnc(c, prefix, opcode, 1, uint8(i.u1), uint8(src), rex)
			c.EmitByte(byte(amount.imm32()))
		default:
			panic("BUG: invalid operand kind")
		}
	case xmmRmiReg:
		const legPrefix = legacyPrefixes0x66
		rex := rexInfo(0).clearW()
		dst := regEncodings[i.op2.reg().RealReg()]

		var opcode uint32
		var regDigit uint8

		op := sseOpcode(i.u1)
		op1 := i.op1
		if i.op1.kind == operandKindImm32 {
			switch op {
			case sseOpcodePsllw:
				opcode, regDigit = 0x0f71, 6
			case sseOpcodePslld:
				opcode, regDigit = 0x0f72, 6
			case sseOpcodePsllq:
				opcode, regDigit = 0x0f73, 6
			case sseOpcodePsraw:
				opcode, regDigit = 0x0f71, 4
			case sseOpcodePsrad:
				opcode, regDigit = 0x0f72, 4
			case sseOpcodePsrlw:
				opcode, regDigit = 0x0f71, 2
			case sseOpcodePsrld:
				opcode, regDigit = 0x0f72, 2
			case sseOpcodePsrlq:
				opcode, regDigit = 0x0f73, 2
			default:
				panic("invalid opcode")
			}

			encodeEncEnc(c, legPrefix, opcode, 2, regDigit, uint8(dst), rex)
			imm32 := op1.imm32()
			if imm32 > 0xff&imm32 {
				panic("immediate value does not fit 1 byte")
			}
			c.EmitByte(uint8(imm32))
		} else {
			switch op {
			case sseOpcodePsllw:
				opcode = 0x0ff1
			case sseOpcodePslld:
				opcode = 0x0ff2
			case sseOpcodePsllq:
				opcode = 0x0ff3
			case sseOpcodePsraw:
				opcode = 0x0fe1
			case sseOpcodePsrad:
				opcode = 0x0fe2
			case sseOpcodePsrlw:
				opcode = 0x0fd1
			case sseOpcodePsrld:
				opcode = 0x0fd2
			case sseOpcodePsrlq:
				opcode = 0x0fd3
			default:
				panic("invalid opcode")
			}

			if op1.kind == operandKindReg {
				reg := regEncodings[op1.reg().RealReg()]
				encodeRegReg(c, legPrefix, opcode, 2, dst, reg, rex)
			} else if op1.kind == operandKindMem {
				m := op1.addressMode()
				encodeRegMem(c, legPrefix, opcode, 2, dst, m, rex)
			} else {
				panic("BUG: invalid operand kind")
			}
		}

	case cmpRmiR:
		var opcode uint32
		isCmp := i.u1 != 0
		rex := rexInfo(0)
		_64 := i.b1
		if _64 { // 64 bit.
			rex = rex.setW()
		} else {
			rex = rex.clearW()
		}
		dst := regEncodings[i.op2.reg().RealReg()]
		op1 := i.op1
		switch op1.kind {
		case operandKindReg:
			reg := regEncodings[op1.reg().RealReg()]
			if isCmp {
				opcode = 0x39
			} else {
				opcode = 0x85
			}
			// Here we swap the encoding of the operands for CMP to be consistent with the output of LLVM/GCC.
			encodeRegReg(c, legacyPrefixesNone, opcode, 1, reg, dst, rex)

		case operandKindMem:
			if isCmp {
				opcode = 0x3b
			} else {
				opcode = 0x85
			}
			m := op1.addressMode()
			encodeRegMem(c, legacyPrefixesNone, opcode, 1, dst, m, rex)

		case operandKindImm32:
			imm32 := op1.imm32()
			useImm8 := isCmp && lower8willSignExtendTo32(imm32)
			var subopcode uint8

			switch {
			case isCmp && useImm8:
				opcode, subopcode = 0x83, 7
			case isCmp && !useImm8:
				opcode, subopcode = 0x81, 7
			default:
				opcode, subopcode = 0xf7, 0
			}
			encodeEncEnc(c, legacyPrefixesNone, opcode, 1, subopcode, uint8(dst), rex)
			if useImm8 {
				c.EmitByte(uint8(imm32))
			} else {
				c.Emit4Bytes(imm32)
			}

		default:
			panic("BUG: invalid operand kind")
		}
	case setcc:
		cc := cond(i.u1)
		dst := regEncodings[i.op2.reg().RealReg()]
		rex := rexInfo(0).clearW().always()
		opcode := uint32(0x0f90) + uint32(cc)
		encodeEncEnc(c, legacyPrefixesNone, opcode, 2, 0, uint8(dst), rex)
	case cmove:
		cc := cond(i.u1)
		dst := regEncodings[i.op2.reg().RealReg()]
		rex := rexInfo(0)
		if i.b1 { // 64 bit.
			rex = rex.setW()
		} else {
			rex = rex.clearW()
		}
		opcode := uint32(0x0f40) + uint32(cc)
		src := i.op1
		switch src.kind {
		case operandKindReg:
			srcReg := regEncodings[src.reg().RealReg()]
			encodeRegReg(c, legacyPrefixesNone, opcode, 2, dst, srcReg, rex)
		case operandKindMem:
			m := src.addressMode()
			encodeRegMem(c, legacyPrefixesNone, opcode, 2, dst, m, rex)
		default:
			panic("BUG: invalid operand kind")
		}
	case push64:
		op := i.op1

		switch op.kind {
		case operandKindReg:
			dst := regEncodings[op.reg().RealReg()]
			if dst.rexBit() > 0 {
				c.EmitByte(rexEncodingDefault | 0x1)
			}
			c.EmitByte(0x50 | dst.encoding())
		case operandKindMem:
			m := op.addressMode()
			encodeRegMem(
				c, legacyPrefixesNone, 0xff, 1, regEnc(6), m, rexInfo(0).clearW(),
			)
		case operandKindImm32:
			c.EmitByte(0x68)
			c.Emit4Bytes(op.imm32())
		default:
			panic("BUG: invalid operand kind")
		}

	case pop64:
		dst := regEncodings[i.op1.reg().RealReg()]
		if dst.rexBit() > 0 {
			c.EmitByte(rexEncodingDefault | 0x1)
		}
		c.EmitByte(0x58 | dst.encoding())

	case xmmMovRM:
		var legPrefix legacyPrefixes
		var opcode uint32
		const opcodeNum = 2
		switch sseOpcode(i.u1) {
		case sseOpcodeMovaps:
			legPrefix, opcode = legacyPrefixesNone, 0x0f29
		case sseOpcodeMovapd:
			legPrefix, opcode = legacyPrefixes0x66, 0x0f29
		case sseOpcodeMovdqa:
			legPrefix, opcode = legacyPrefixes0x66, 0x0f7f
		case sseOpcodeMovdqu:
			legPrefix, opcode = legacyPrefixes0xF3, 0x0f7f
		case sseOpcodeMovss:
			legPrefix, opcode = legacyPrefixes0xF3, 0x0f11
		case sseOpcodeMovsd:
			legPrefix, opcode = legacyPrefixes0xF2, 0x0f11
		case sseOpcodeMovups:
			legPrefix, opcode = legacyPrefixesNone, 0x0f11
		case sseOpcodeMovupd:
			legPrefix, opcode = legacyPrefixes0x66, 0x0f11
		default:
			panic(fmt.Sprintf("Unsupported sseOpcode: %s", sseOpcode(i.u1)))
		}

		dst := regEncodings[i.op1.reg().RealReg()]
		encodeRegMem(c, legPrefix, opcode, opcodeNum, dst, i.op2.addressMode(), rexInfo(0).clearW())
	case xmmLoadConst:
		// A pooled 128-bit constant is reached through an ordinary memory
		// operand (in practice a RIP-relative reference to the pool machine.Encode
		// appends after the function body), so this is byte for byte the memory
		// form of xmmUnaryRmR: MOVUPS xmm1, m128 is `0F 10 /r`, MOVAPS is
		// `0F 28 /r` and MOVDQU is `F3 0F 6F /r` (Intel SDM Vol.2B).
		//
		// Rewriting the instruction in place, the way `zeros` does below, is not
		// only shorter: it also makes the RIP-relative displacement resolvable,
		// because machine.Encode dispatches the pending fixups on the kind of the
		// instruction that requested them and it knows xmmUnaryRmR.
		if i.op1.kind != operandKindMem {
			panic("BUG: xmmLoadConst must address the constant through memory")
		}
		needsLabelResolution = i.asXmmUnaryRmR(sseOpcode(i.u1), i.op1, i.op2.reg()).encode(c)

	case xmmToGpr:
		var legPrefix legacyPrefixes
		var opcode uint32
		var argSwap bool
		const opcodeNum = 2
		switch sseOpcode(i.u1) {
		case sseOpcodeMovd, sseOpcodeMovq:
			legPrefix, opcode, argSwap = legacyPrefixes0x66, 0x0f7e, false
		case sseOpcodeMovmskps:
			legPrefix, opcode, argSwap = legacyPrefixesNone, 0x0f50, true
		case sseOpcodeMovmskpd:
			legPrefix, opcode, argSwap = legacyPrefixes0x66, 0x0f50, true
		case sseOpcodePmovmskb:
			legPrefix, opcode, argSwap = legacyPrefixes0x66, 0x0fd7, true
		case sseOpcodeCvttss2si:
			legPrefix, opcode, argSwap = legacyPrefixes0xF3, 0x0f2c, true
		case sseOpcodeCvttsd2si:
			legPrefix, opcode, argSwap = legacyPrefixes0xF2, 0x0f2c, true
		default:
			panic(fmt.Sprintf("Unsupported sseOpcode: %s", sseOpcode(i.u1)))
		}

		var rex rexInfo
		if i.b1 {
			rex = rex.setW()
		} else {
			rex = rex.clearW()
		}
		src := regEncodings[i.op1.reg().RealReg()]
		dst := regEncodings[i.op2.reg().RealReg()]
		if argSwap {
			src, dst = dst, src
		}
		encodeRegReg(c, legPrefix, opcode, opcodeNum, src, dst, rex)

	case cvtUint64ToFloatSeq:
		src, dst, tmpGp, tmpGp2, dst64 := i.cvtUint64ToFloatSeqData()

		var cvtOp, addOp sseOpcode
		if dst64 {
			cvtOp, addOp = sseOpcodeCvtsi2sd, sseOpcodeAddsd
		} else {
			cvtOp, addOp = sseOpcodeCvtsi2ss, sseOpcodeAddss
		}

		// cvtsi2s{s,d} reads its source as *signed*, so only the values whose
		// sign bit is clear convert directly. For the others we convert half the
		// value and double the result, keeping the discarded low bit sticky
		// (`(v>>1)|(v&1)`) so that the single rounding step still rounds the way
		// it would have on the full value. This is the sequence
		// machine.lowerFcvtFromUint emits for a 64-bit source.
		//
		//	testq %src, %src
		//	js    negative
		//	cvtsi2s{s,d}q %src, %dst
		//	jmp   done
		// negative:
		//	movq  %src, %tmpGp
		//	shrq  $1, %tmpGp
		//	movq  %src, %tmpGp2
		//	andq  $1, %tmpGp2
		//	orq   %tmpGp2, %tmpGp
		//	cvtsi2s{s,d}q %tmpGp, %dst
		//	add{ss,sd} %dst, %dst
		// done:
		newScratchInstr().asCmpRmiR(false, newOperandReg(src), src, true).encode(c)
		toNegative := emitJccRel32(c, condS)
		newScratchInstr().asGprToXmm(cvtOp, newOperandReg(src), dst, true).encode(c)
		toDone := emitJmpRel32(c)

		bindRel32(c, toNegative)
		newScratchInstr().asMovRR(src, tmpGp, true).encode(c)
		newScratchInstr().asShiftR(shiftROpShiftRightLogical, newOperandImm32(1), tmpGp, true).encode(c)
		newScratchInstr().asMovRR(src, tmpGp2, true).encode(c)
		newScratchInstr().asAluRmiR(aluRmiROpcodeAnd, newOperandImm32(1), tmpGp2, true).encode(c)
		newScratchInstr().asAluRmiR(aluRmiROpcodeOr, newOperandReg(tmpGp2), tmpGp, true).encode(c)
		newScratchInstr().asGprToXmm(cvtOp, newOperandReg(tmpGp), dst, true).encode(c)
		newScratchInstr().asXmmRmR(addOp, newOperandReg(dst), dst).encode(c)

		bindRel32(c, toDone)

	case cvtFloatToSintSeq:
		execCtx, src, tmpGp, tmpGp2, tmpXmm, src64, dst64, sat := i.fcvtToSintSequenceData()

		var cmpOp, truncOp sseOpcode
		if src64 {
			cmpOp, truncOp = sseOpcodeUcomisd, sseOpcodeCvttsd2si
		} else {
			cmpOp, truncOp = sseOpcodeUcomiss, sseOpcodeCvttss2si
		}

		// cvtts{s,d}2si returns the "integer indefinite" value (INT_MIN) for
		// every input it cannot represent: NaN, out of range in either
		// direction. The sequence below re-examines the source to tell those
		// apart, and either saturates or traps. It is the same sequence
		// machine.lowerFcvtToSintSequenceAfterRegalloc emits.
		var toDone []int

		newScratchInstr().asXmmToGpr(truncOp, src, tmpGp, dst64).encode(c)

		// INT_MIN is the only result for which `cmp $1, %dst` overflows, so OF
		// clear means the conversion was exact.
		newScratchInstr().asCmpRmiR(true, newOperandImm32(1), tmpGp, dst64).encode(c)
		toDone = append(toDone, emitJccRel32(c, condNO))

		// Comparing the source with itself sets PF exactly when it is NaN.
		newScratchInstr().asXmmCmpRmR(cmpOp, newOperandReg(src), src).encode(c)
		toNotNaN := emitJccRel32(c, condNP)

		if sat {
			// NaN saturates to zero.
			newScratchInstr().asZeros(tmpGp).encode(c)
			toDone = append(toDone, emitJmpRel32(c))

			bindRel32(c, toNotNaN)

			// Not NaN: a source below zero underflowed and INT_MIN is already
			// the right answer, anything else overflowed and saturates to
			// INT_MAX.
			newScratchInstr().asZeros(tmpXmm).encode(c)
			newScratchInstr().asXmmCmpRmR(cmpOp, newOperandReg(tmpXmm), src).encode(c)
			toDone = append(toDone, emitJccRel32(c, condB))

			if dst64 {
				emitIconst(c, tmpGp, math.MaxInt64, dst64)
			} else {
				emitIconst(c, tmpGp, math.MaxInt32, dst64)
			}
		} else {
			// NaN traps.
			emitExitWithCode(c, execCtx, nativeapi.ExitCodeInvalidConversionToInteger)

			bindRel32(c, toNotNaN)

			// Not NaN, so this is an overflow unless the source is exactly the
			// minimum representable integer. The magic constants below are that
			// minimum for int[32|64] expressed as a float[32|64].
			condAboveThreshold := condNB
			var minInt uint64
			switch {
			case src64 && dst64:
				minInt = 0xc3e0000000000000
			case src64 && !dst64:
				condAboveThreshold = condNBE
				minInt = 0xC1E0_0000_0020_0000
			case !src64 && dst64:
				minInt = 0xDF00_0000
			case !src64 && !dst64:
				minInt = 0xCF00_0000
			}

			newScratchInstr().asImm(tmpGp2, minInt, src64).encode(c)
			newScratchInstr().asGprToXmm(sseOpcodeMovq, newOperandReg(tmpGp2), tmpXmm, src64).encode(c)
			newScratchInstr().asXmmCmpRmR(cmpOp, newOperandReg(tmpXmm), src).encode(c)
			toCheckPositive := emitJccRel32(c, condAboveThreshold)

			emitExitWithCode(c, execCtx, nativeapi.ExitCodeIntegerOverflow)

			bindRel32(c, toCheckPositive)

			// The source is at or above the threshold: it is only in range if
			// it is negative, i.e. if it is the minimum integer itself.
			newScratchInstr().asXmmRmR(sseOpcodeXorpd, newOperandReg(tmpXmm), tmpXmm).encode(c)
			newScratchInstr().asXmmCmpRmR(cmpOp, newOperandReg(src), tmpXmm).encode(c)
			toDone = append(toDone, emitJccRel32(c, condNB))

			emitExitWithCode(c, execCtx, nativeapi.ExitCodeIntegerOverflow)
		}

		for _, at := range toDone {
			bindRel32(c, at)
		}

	case cvtFloatToUintSeq:
		execCtx, src, tmpGp, tmpGp2, tmpXmm, tmpXmm2, src64, dst64, sat := i.fcvtToUintSequenceData()

		var subOp, cmpOp, truncOp sseOpcode
		if src64 {
			subOp, cmpOp, truncOp = sseOpcodeSubsd, sseOpcodeUcomisd, sseOpcodeCvttsd2si
		} else {
			subOp, cmpOp, truncOp = sseOpcodeSubss, sseOpcodeUcomiss, sseOpcodeCvttss2si
		}

		// There is no unsigned truncation instruction, so the source is split at
		// 2**(dstBits-1): below the threshold a plain signed truncation is
		// already correct, above it we subtract the threshold, truncate, and add
		// it back on the integer side. Same sequence as
		// machine.lowerFcvtToUintSequenceAfterRegalloc.
		var toDone []int

		// tmpXmm = 2**(dstBits-1), expressed in the source float format.
		switch {
		case src64 && dst64:
			newScratchInstr().asImm(tmpGp, 0x43e0000000000000, true).encode(c)
			newScratchInstr().asGprToXmm(sseOpcodeMovq, newOperandReg(tmpGp), tmpXmm, true).encode(c)
		case src64 && !dst64:
			newScratchInstr().asImm(tmpGp, 0x41e0000000000000, true).encode(c)
			newScratchInstr().asGprToXmm(sseOpcodeMovq, newOperandReg(tmpGp), tmpXmm, true).encode(c)
		case !src64 && dst64:
			newScratchInstr().asImm(tmpGp, 0x5f000000, false).encode(c)
			newScratchInstr().asGprToXmm(sseOpcodeMovq, newOperandReg(tmpGp), tmpXmm, false).encode(c)
		case !src64 && !dst64:
			newScratchInstr().asImm(tmpGp, 0x4f000000, false).encode(c)
			newScratchInstr().asGprToXmm(sseOpcodeMovq, newOperandReg(tmpGp), tmpXmm, false).encode(c)
		}

		newScratchInstr().asXmmCmpRmR(cmpOp, newOperandReg(tmpXmm), src).encode(c)
		toAboveThreshold := emitJccRel32(c, condNB)
		toNotNaN := emitJccRel32(c, condNP)

		// Below the threshold and unordered, so the source is NaN.
		if sat {
			newScratchInstr().asZeros(tmpGp).encode(c)
			toDone = append(toDone, emitJmpRel32(c))
		} else {
			emitExitWithCode(c, execCtx, nativeapi.ExitCodeInvalidConversionToInteger)
		}

		bindRel32(c, toNotNaN)

		// Below the threshold and ordered: a signed truncation is exact, and a
		// negative result means the source was below zero.
		newScratchInstr().asXmmToGpr(truncOp, src, tmpGp, dst64).encode(c)
		newScratchInstr().asCmpRmiR(true, newOperandImm32(0), tmpGp, dst64).encode(c)
		toDone = append(toDone, emitJccRel32(c, condNL))

		if sat {
			// Underflow saturates to the minimum unsigned value, zero.
			newScratchInstr().asZeros(tmpGp).encode(c)
			toDone = append(toDone, emitJmpRel32(c))
		} else {
			emitExitWithCode(c, execCtx, nativeapi.ExitCodeIntegerOverflow)
		}

		bindRel32(c, toAboveThreshold)

		// At or above the threshold: truncate (source - threshold) instead.
		newScratchInstr().asXmmUnaryRmR(sseOpcodeMovdqu, newOperandReg(src), tmpXmm2).encode(c)
		newScratchInstr().asXmmRmR(subOp, newOperandReg(tmpXmm), tmpXmm2).encode(c)
		newScratchInstr().asXmmToGpr(truncOp, tmpXmm2, tmpGp, dst64).encode(c)
		newScratchInstr().asCmpRmiR(true, newOperandImm32(0), tmpGp, dst64).encode(c)
		toNextLarge := emitJccRel32(c, condNL)

		if sat {
			// The shifted value still did not fit, so the source was above the
			// maximum: saturate to it.
			var maxInt uint64
			if dst64 {
				maxInt = math.MaxUint64
			} else {
				maxInt = math.MaxUint32
			}
			emitIconst(c, tmpGp, maxInt, dst64)
			toDone = append(toDone, emitJmpRel32(c))
		} else {
			emitExitWithCode(c, execCtx, nativeapi.ExitCodeIntegerOverflow)
		}

		bindRel32(c, toNextLarge)

		// Add the threshold back on, which wraps into the unsigned top half.
		var addend operand
		if dst64 {
			emitIconst(c, tmpGp2, 0x8000000000000000, true)
			addend = newOperandReg(tmpGp2)
		} else {
			addend = newOperandImm32(0x80000000)
		}
		newScratchInstr().asAluRmiR(aluRmiROpcodeAdd, addend, tmpGp, dst64).encode(c)

		for _, at := range toDone {
			bindRel32(c, at)
		}

	case xmmMinMaxSeq:
		isMin, _64 := i.u1 != 0, i.b1
		lhs, rhsDst := i.op1.reg(), i.op2.reg()

		var cmpOp, minMaxOp, signOp, addOp sseOpcode
		switch {
		case _64 && isMin:
			cmpOp, minMaxOp, signOp, addOp = sseOpcodeUcomisd, sseOpcodeMinsd, sseOpcodeOrpd, sseOpcodeAddsd
		case _64 && !isMin:
			cmpOp, minMaxOp, signOp, addOp = sseOpcodeUcomisd, sseOpcodeMaxsd, sseOpcodeAndpd, sseOpcodeAddsd
		case !_64 && isMin:
			cmpOp, minMaxOp, signOp, addOp = sseOpcodeUcomiss, sseOpcodeMinss, sseOpcodeOrps, sseOpcodeAddss
		case !_64 && !isMin:
			cmpOp, minMaxOp, signOp, addOp = sseOpcodeUcomiss, sseOpcodeMaxss, sseOpcodeAndps, sseOpcodeAddss
		}

		// mins{s,d}/maxs{s,d} return the second operand whenever the two are
		// unordered or both zero, which is neither the IEEE nor the WebAssembly
		// answer. ucomis{s,d} separates the three cases (see
		// https://www.felixcloutier.com/x86/ucomiss#operation): ZF clear means
		// ordered and different, ZF and PF set means unordered, ZF set with PF
		// clear means equal. Same shape as machine.lowerFminFmax.
		//
		//	ucomis{s,d} %lhs, %rhsDst
		//	jnz  doMinMax
		//	jp   isNaN
		//	{or,and}p{s,d} %lhs, %rhsDst
		//	jmp  done
		// isNaN:
		//	add{ss,sd} %lhs, %rhsDst
		//	jmp  done
		// doMinMax:
		//	{min,max}s{s,d} %lhs, %rhsDst
		// done:
		newScratchInstr().asXmmCmpRmR(cmpOp, newOperandReg(lhs), rhsDst).encode(c)
		toDoMinMax := emitJccRel32(c, condNZ)
		toIsNaN := emitJccRel32(c, condP)

		// Equal: the operands can still be +0 and -0, and ORing the sign bits
		// gives -0 for min while ANDing them gives +0 for max.
		newScratchInstr().asXmmRmR(signOp, newOperandReg(lhs), rhsDst).encode(c)
		toDoneFromEqual := emitJmpRel32(c)

		// Unordered: adding the operands quiets whichever one is the NaN and
		// propagates it.
		bindRel32(c, toIsNaN)
		newScratchInstr().asXmmRmR(addOp, newOperandReg(lhs), rhsDst).encode(c)
		toDoneFromNaN := emitJmpRel32(c)

		// Ordered and different: the hardware instruction is exact.
		bindRel32(c, toDoMinMax)
		newScratchInstr().asXmmRmR(minMaxOp, newOperandReg(lhs), rhsDst).encode(c)

		bindRel32(c, toDoneFromEqual)
		bindRel32(c, toDoneFromNaN)

	case xmmCmpRmR:
		var prefix legacyPrefixes
		var opcode uint32
		var opcodeNum uint32
		rex := rexInfo(0)
		_64 := i.b1
		if _64 { // 64 bit.
			rex = rex.setW()
		} else {
			rex = rex.clearW()
		}

		op := sseOpcode(i.u1)
		switch op {
		case sseOpcodePtest:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0f3817, 3
		case sseOpcodeUcomisd:
			prefix, opcode, opcodeNum = legacyPrefixes0x66, 0x0f2e, 2
		case sseOpcodeUcomiss:
			prefix, opcode, opcodeNum = legacyPrefixesNone, 0x0f2e, 2
		default:
			panic(fmt.Sprintf("Unsupported sseOpcode: %s", op))
		}

		dst := regEncodings[i.op2.reg().RealReg()]
		op1 := i.op1
		switch op1.kind {
		case operandKindReg:
			reg := regEncodings[op1.reg().RealReg()]
			encodeRegReg(c, prefix, opcode, opcodeNum, dst, reg, rex)

		case operandKindMem:
			m := op1.addressMode()
			encodeRegMem(c, prefix, opcode, opcodeNum, dst, m, rex)

		default:
			panic("BUG: invalid operand kind")
		}
	case xmmRmRImm:
		op := sseOpcode(i.u1)
		var legPrex legacyPrefixes
		var opcode uint32
		var opcodeNum uint32
		var swap bool
		switch op {
		case sseOpcodeCmpps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0FC2, 2
		case sseOpcodeCmppd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FC2, 2
		case sseOpcodeCmpss:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF3, 0x0FC2, 2
		case sseOpcodeCmpsd:
			legPrex, opcode, opcodeNum = legacyPrefixes0xF2, 0x0FC2, 2
		case sseOpcodeInsertps:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3A21, 3
		case sseOpcodePalignr:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3A0F, 3
		case sseOpcodePinsrb:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3A20, 3
		case sseOpcodePinsrw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FC4, 2
		case sseOpcodePinsrd, sseOpcodePinsrq:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3A22, 3
		case sseOpcodePextrb:
			swap = true
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3A14, 3
		case sseOpcodePextrw:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0FC5, 2
		case sseOpcodePextrd, sseOpcodePextrq:
			swap = true
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3A16, 3
		case sseOpcodePshufd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F70, 2
		case sseOpcodeRoundps:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3A08, 3
		case sseOpcodeRoundpd:
			legPrex, opcode, opcodeNum = legacyPrefixes0x66, 0x0F3A09, 3
		case sseOpcodeShufps:
			legPrex, opcode, opcodeNum = legacyPrefixesNone, 0x0FC6, 2
		default:
			panic(fmt.Sprintf("Unsupported sseOpcode: %s", op))
		}

		dst := regEncodings[i.op2.reg().RealReg()]

		var rex rexInfo
		if op == sseOpcodePextrq || op == sseOpcodePinsrq {
			rex = rexInfo(0).setW()
		} else {
			rex = rexInfo(0).clearW()
		}
		op1 := i.op1
		if op1.kind == operandKindReg {
			src := regEncodings[op1.reg().RealReg()]
			if swap {
				src, dst = dst, src
			}
			encodeRegReg(c, legPrex, opcode, opcodeNum, dst, src, rex)
		} else if i.op1.kind == operandKindMem {
			if swap {
				panic("BUG: this is not possible to encode")
			}
			m := i.op1.addressMode()
			encodeRegMem(c, legPrex, opcode, opcodeNum, dst, m, rex)
		} else {
			panic("BUG: invalid operand kind")
		}

		c.EmitByte(byte(i.u2))

	case jmp:
		const (
			regMemOpcode    = 0xff
			regMemOpcodeNum = 1
			regMemSubOpcode = 4
		)
		op := i.op1
		switch op.kind {
		case operandKindLabel:
			needsLabelResolution = true
			fallthrough
		case operandKindImm32:
			c.EmitByte(0xe9)
			c.Emit4Bytes(op.imm32())
		case operandKindMem:
			m := op.addressMode()
			encodeRegMem(c,
				legacyPrefixesNone,
				regMemOpcode, regMemOpcodeNum,
				regMemSubOpcode, m, rexInfo(0).clearW(),
			)
		case operandKindReg:
			r := op.reg().RealReg()
			encodeRegReg(
				c,
				legacyPrefixesNone,
				regMemOpcode, regMemOpcodeNum,
				regMemSubOpcode,
				regEncodings[r], rexInfo(0).clearW(),
			)
		default:
			panic("BUG: invalid operand kind")
		}

	case jmpIf:
		op := i.op1
		switch op.kind {
		case operandKindLabel:
			needsLabelResolution = true
			fallthrough
		case operandKindImm32:
			c.EmitByte(0x0f)
			c.EmitByte(0x80 | cond(i.u1).encoding())
			c.Emit4Bytes(op.imm32())
		default:
			panic("BUG: invalid operand kind")
		}

	case jmpTableIsland:
		needsLabelResolution = true
		for tc := uint64(0); tc < i.u2; tc++ {
			c.Emit8Bytes(0)
		}

	case exitSequence:
		execCtx := i.op1.reg()
		allocatedAmode := i.op2.addressMode()

		// Restore the RBP, RSP, and return to the Go code:
		*allocatedAmode = amode{
			kindWithShift: uint32(amodeImmReg), base: execCtx,
			imm32: nativeapi.ExecutionContextOffsetOriginalFramePointer.U32(),
		}
		encodeLoad64(c, allocatedAmode, rbp)
		allocatedAmode.imm32 = nativeapi.ExecutionContextOffsetOriginalStackPointer.U32()
		encodeLoad64(c, allocatedAmode, rsp)
		encodeRet(c)

	case ud2:
		c.EmitByte(0x0f)
		c.EmitByte(0x0b)

	case call:
		c.EmitByte(0xe8)
		// Meaning that the call target is a function value, and requires relocation.
		c.AddRelocationInfo(ssa.FuncRef(i.u1), false)
		// Note that this is zero as a placeholder for the call target if it's a function value.
		c.Emit4Bytes(uint32(i.u2))

	case callIndirect:
		op := i.op1

		const opcodeNum = 1
		const opcode = 0xff
		rex := rexInfo(0).clearW()
		switch op.kind {
		case operandKindReg:
			dst := regEncodings[op.reg().RealReg()]
			encodeRegReg(c,
				legacyPrefixesNone,
				opcode, opcodeNum,
				regEnc(2),
				dst,
				rex,
			)
		case operandKindMem:
			m := op.addressMode()
			encodeRegMem(c,
				legacyPrefixesNone,
				opcode, opcodeNum,
				regEnc(2),
				m,
				rex,
			)
		default:
			panic("BUG: invalid operand kind")
		}

	case tailCall:
		// Encode as jmp.
		c.EmitByte(0xe9)
		// Meaning that the call target is a function value, and requires relocation.
		c.AddRelocationInfo(ssa.FuncRef(i.u1), true)
		// Note that this is zero as a placeholder for the call target if it's a function value.
		c.Emit4Bytes(uint32(i.u2))

	case tailCallIndirect:
		op := i.op1

		const opcodeNum = 1
		const opcode = 0xff
		const regMemSubOpcode = 4
		rex := rexInfo(0).clearW()
		switch op.kind {
		// Indirect tail calls always take a register as the target.
		// Note: the register should be a callee-saved register (usually r11).
		case operandKindReg:
			dst := regEncodings[op.reg().RealReg()]
			encodeRegReg(c,
				legacyPrefixesNone,
				opcode, opcodeNum,
				regMemSubOpcode,
				dst,
				rex,
			)
		default:
			panic("BUG: invalid operand kind")
		}

	case xchg:
		src, dst := regEncodings[i.op1.reg().RealReg()], i.op2
		size := i.u1

		var rex rexInfo
		var opcode uint32
		lp := legacyPrefixesNone
		switch size {
		case 8:
			opcode = 0x87
			rex = rexInfo(0).setW()
		case 4:
			opcode = 0x87
			rex = rexInfo(0).clearW()
		case 2:
			lp = legacyPrefixes0x66
			opcode = 0x87
			rex = rexInfo(0).clearW()
		case 1:
			opcode = 0x86
			// XCHG r/m8, r8 is `86 /r` (Intel SDM Vol.2B, XCHG). A byte
			// operand numbered 4 to 7 names AH/CH/DH/BH when no REX prefix is
			// present and SPL/BPL/SIL/DIL when one is, so a REX prefix has to
			// be forced for either operand that lands in that range: the
			// ModRM.reg operand (src) and, for the register-register form, the
			// ModRM.rm operand (dst) as well. `xchgb %sil, %dil` is 40 86 F7;
			// the same bytes without the prefix, 86 F7, swap %dh and %bh.
			if e := src.encoding(); e >= 4 && e <= 7 {
				rex = rexInfo(0).always()
			}
			if dst.kind == operandKindReg {
				if e := regEncodings[dst.reg().RealReg()].encoding(); e >= 4 && e <= 7 {
					rex = rexInfo(0).always()
				}
			}
		default:
			panic(fmt.Sprintf("BUG: invalid size %d: %s", size, i.String()))
		}

		switch dst.kind {
		case operandKindMem:
			m := dst.addressMode()
			encodeRegMem(c, lp, opcode, 1, src, m, rex)
		case operandKindReg:
			r := dst.reg().RealReg()
			encodeRegReg(c, lp, opcode, 1, src, regEncodings[r], rex)
		default:
			panic("BUG: invalid operand kind")
		}

	case lockcmpxchg:
		src, dst := regEncodings[i.op1.reg().RealReg()], i.op2
		size := i.u1

		var rex rexInfo
		var opcode uint32
		lp := legacyPrefixes0xF0 // Lock prefix.
		switch size {
		case 8:
			opcode = 0x0FB1
			rex = rexInfo(0).setW()
		case 4:
			opcode = 0x0FB1
			rex = rexInfo(0).clearW()
		case 2:
			lp = legacyPrefixes0x660xF0 // Legacy prefix + Lock prefix.
			opcode = 0x0FB1
			rex = rexInfo(0).clearW()
		case 1:
			opcode = 0x0FB0
			// Some destinations must be encoded with REX.R = 1.
			if e := src.encoding(); e >= 4 && e <= 7 {
				rex = rexInfo(0).always()
			}
		default:
			panic(fmt.Sprintf("BUG: invalid size %d: %s", size, i.String()))
		}

		switch dst.kind {
		case operandKindMem:
			m := dst.addressMode()
			encodeRegMem(c, lp, opcode, 2, src, m, rex)
		default:
			panic("BUG: invalid operand kind")
		}

	case lockxadd:
		src, dst := regEncodings[i.op1.reg().RealReg()], i.op2
		size := i.u1

		var rex rexInfo
		var opcode uint32
		lp := legacyPrefixes0xF0 // Lock prefix.
		switch size {
		case 8:
			opcode = 0x0FC1
			rex = rexInfo(0).setW()
		case 4:
			opcode = 0x0FC1
			rex = rexInfo(0).clearW()
		case 2:
			lp = legacyPrefixes0x660xF0 // Legacy prefix + Lock prefix.
			opcode = 0x0FC1
			rex = rexInfo(0).clearW()
		case 1:
			opcode = 0x0FC0
			// Some destinations must be encoded with REX.R = 1.
			if e := src.encoding(); e >= 4 && e <= 7 {
				rex = rexInfo(0).always()
			}
		default:
			panic(fmt.Sprintf("BUG: invalid size %d: %s", size, i.String()))
		}

		switch dst.kind {
		case operandKindMem:
			m := dst.addressMode()
			encodeRegMem(c, lp, opcode, 2, src, m, rex)
		default:
			panic("BUG: invalid operand kind")
		}

	case zeros:
		r := i.op2.reg()
		if r.RegType() == regalloc.RegTypeInt {
			i.asAluRmiR(aluRmiROpcodeXor, newOperandReg(r), r, true)
		} else {
			i.asXmmRmR(sseOpcodePxor, newOperandReg(r), r)
		}
		i.encode(c)

	case mfence:
		// https://www.felixcloutier.com/x86/mfence
		c.EmitByte(0x0f)
		c.EmitByte(0xae)
		c.EmitByte(0xf0)

	default:
		panic(fmt.Sprintf("TODO: %v", i.kind))
	}
	return
}

func encodeLoad64(c backend.Compiler, m *amode, rd regalloc.RealReg) {
	dst := regEncodings[rd]
	encodeRegMem(c, legacyPrefixesNone, 0x8b, 1, dst, m, rexInfo(0).setW())
}

func encodeRet(c backend.Compiler) {
	c.EmitByte(0xc3)
}

func encodeEncEnc(
	c backend.Compiler,
	legPrefixes legacyPrefixes,
	opcodes uint32,
	opcodeNum uint32,
	r uint8,
	rm uint8,
	rex rexInfo,
) {
	legPrefixes.encode(c)
	rex.encode(c, r>>3, rm>>3)

	for opcodeNum > 0 {
		opcodeNum--
		c.EmitByte(byte((opcodes >> (opcodeNum << 3)) & 0xff))
	}
	c.EmitByte(encodeModRM(3, r&7, rm&7))
}

func encodeRegReg(
	c backend.Compiler,
	legPrefixes legacyPrefixes,
	opcodes uint32,
	opcodeNum uint32,
	r regEnc,
	rm regEnc,
	rex rexInfo,
) {
	encodeEncEnc(c, legPrefixes, opcodes, opcodeNum, uint8(r), uint8(rm), rex)
}

func encodeModRM(mod byte, reg byte, rm byte) byte {
	return mod<<6 | reg<<3 | rm
}

func encodeSIB(shift byte, encIndex byte, encBase byte) byte {
	return shift<<6 | encIndex<<3 | encBase
}

func encodeRegMem(
	c backend.Compiler, legPrefixes legacyPrefixes, opcodes uint32, opcodeNum uint32, r regEnc, m *amode, rex rexInfo,
) (needsLabelResolution bool) {
	needsLabelResolution = encodeEncMem(c, legPrefixes, opcodes, opcodeNum, uint8(r), m, rex)
	return
}

func encodeEncMem(
	c backend.Compiler, legPrefixes legacyPrefixes, opcodes uint32, opcodeNum uint32, r uint8, m *amode, rex rexInfo,
) (needsLabelResolution bool) {
	legPrefixes.encode(c)

	const (
		modNoDisplacement    = 0b00
		modShortDisplacement = 0b01
		modLongDisplacement  = 0b10

		useSBI = 4 // the encoding of rsp or r12 register.
	)

	switch m.kind() {
	case amodeImmReg, amodeImmRBP:
		base := m.base.RealReg()
		baseEnc := regEncodings[base]

		rex.encode(c, regRexBit(r), baseEnc.rexBit())

		for opcodeNum > 0 {
			opcodeNum--
			c.EmitByte(byte((opcodes >> (opcodeNum << 3)) & 0xff))
		}

		// SIB byte is the last byte of the memory encoding before the displacement
		const sibByte = 0x24 // == encodeSIB(0, 4, 4)

		immZero, baseRbp, baseR13 := m.imm32 == 0, base == rbp, base == r13
		short := lower8willSignExtendTo32(m.imm32)
		rspOrR12 := base == rsp || base == r12

		if immZero && !baseRbp && !baseR13 { // rbp or r13 can't be used as base for without displacement encoding.
			c.EmitByte(encodeModRM(modNoDisplacement, regEncoding(r), baseEnc.encoding()))
			if rspOrR12 {
				c.EmitByte(sibByte)
			}
		} else if short { // Note: this includes the case where m.imm32 == 0 && base == rbp || base == r13.
			c.EmitByte(encodeModRM(modShortDisplacement, regEncoding(r), baseEnc.encoding()))
			if rspOrR12 {
				c.EmitByte(sibByte)
			}
			c.EmitByte(byte(m.imm32))
		} else {
			c.EmitByte(encodeModRM(modLongDisplacement, regEncoding(r), baseEnc.encoding()))
			if rspOrR12 {
				c.EmitByte(sibByte)
			}
			c.Emit4Bytes(m.imm32)
		}

	case amodeRegRegShift:
		base := m.base.RealReg()
		baseEnc := regEncodings[base]
		index := m.index.RealReg()
		indexEnc := regEncodings[index]

		if index == rsp {
			panic("BUG: rsp can't be used as index of addressing mode")
		}

		rex.encodeForIndex(c, regEnc(r), indexEnc, baseEnc)

		for opcodeNum > 0 {
			opcodeNum--
			c.EmitByte(byte((opcodes >> (opcodeNum << 3)) & 0xff))
		}

		immZero, baseRbp, baseR13 := m.imm32 == 0, base == rbp, base == r13
		if immZero && !baseRbp && !baseR13 { // rbp or r13 can't be used as base for without displacement encoding. (curious why? because it's interpreted as RIP relative addressing).
			c.EmitByte(encodeModRM(modNoDisplacement, regEncoding(r), useSBI))
			c.EmitByte(encodeSIB(m.shift(), indexEnc.encoding(), baseEnc.encoding()))
		} else if lower8willSignExtendTo32(m.imm32) {
			c.EmitByte(encodeModRM(modShortDisplacement, regEncoding(r), useSBI))
			c.EmitByte(encodeSIB(m.shift(), indexEnc.encoding(), baseEnc.encoding()))
			c.EmitByte(byte(m.imm32))
		} else {
			c.EmitByte(encodeModRM(modLongDisplacement, regEncoding(r), useSBI))
			c.EmitByte(encodeSIB(m.shift(), indexEnc.encoding(), baseEnc.encoding()))
			c.Emit4Bytes(m.imm32)
		}

	case amodeRipRel:
		rex.encode(c, regRexBit(r), 0)
		for opcodeNum > 0 {
			opcodeNum--
			c.EmitByte(byte((opcodes >> (opcodeNum << 3)) & 0xff))
		}

		// Indicate "LEAQ [RIP + 32bit displacement].
		// https://wiki.osdev.org/X86-64_Instruction_Encoding#32.2F64-bit_addressing
		c.EmitByte(encodeModRM(0b00, regEncoding(r), 0b101))

		// This will be resolved later, so we just emit a placeholder.
		needsLabelResolution = true
		c.Emit4Bytes(0)

	default:
		panic("BUG: invalid addressing mode")
	}
	return
}

const (
	rexEncodingDefault byte = 0x40
	rexEncodingW            = rexEncodingDefault | 0x08
)

// rexInfo is a bit set to indicate:
//
//	0x01: W bit must be cleared.
//	0x02: REX prefix must be emitted.
type rexInfo byte

func (ri rexInfo) setW() rexInfo {
	return ri | 0x01
}

func (ri rexInfo) clearW() rexInfo {
	return ri & 0x02
}

func (ri rexInfo) always() rexInfo {
	return ri | 0x02
}

func (ri rexInfo) notAlways() rexInfo { //nolint
	return ri & 0x01
}

func (ri rexInfo) encode(c backend.Compiler, r uint8, b uint8) {
	var w byte = 0
	if ri&0x01 != 0 {
		w = 0x01
	}
	rex := rexEncodingDefault | w<<3 | r<<2 | b
	if rex != rexEncodingDefault || ri&0x02 != 0 {
		c.EmitByte(rex)
	}
}

func (ri rexInfo) encodeForIndex(c backend.Compiler, encR regEnc, encIndex regEnc, encBase regEnc) {
	var w byte = 0
	if ri&0x01 != 0 {
		w = 0x01
	}
	r := encR.rexBit()
	x := encIndex.rexBit()
	b := encBase.rexBit()
	rex := byte(0x40) | w<<3 | r<<2 | x<<1 | b
	if rex != 0x40 || ri&0x02 != 0 {
		c.EmitByte(rex)
	}
}

type regEnc byte

func (r regEnc) rexBit() byte {
	return regRexBit(byte(r))
}

func (r regEnc) encoding() byte {
	return regEncoding(byte(r))
}

func regRexBit(r byte) byte {
	return r >> 3
}

func regEncoding(r byte) byte {
	return r & 0x07
}

var regEncodings = [...]regEnc{
	rax:   0b000,
	rcx:   0b001,
	rdx:   0b010,
	rbx:   0b011,
	rsp:   0b100,
	rbp:   0b101,
	rsi:   0b110,
	rdi:   0b111,
	r8:    0b1000,
	r9:    0b1001,
	r10:   0b1010,
	r11:   0b1011,
	r12:   0b1100,
	r13:   0b1101,
	r14:   0b1110,
	r15:   0b1111,
	xmm0:  0b000,
	xmm1:  0b001,
	xmm2:  0b010,
	xmm3:  0b011,
	xmm4:  0b100,
	xmm5:  0b101,
	xmm6:  0b110,
	xmm7:  0b111,
	xmm8:  0b1000,
	xmm9:  0b1001,
	xmm10: 0b1010,
	xmm11: 0b1011,
	xmm12: 0b1100,
	xmm13: 0b1101,
	xmm14: 0b1110,
	xmm15: 0b1111,
}

type legacyPrefixes byte

const (
	legacyPrefixesNone legacyPrefixes = iota
	legacyPrefixes0x66
	legacyPrefixes0xF0
	legacyPrefixes0x660xF0
	legacyPrefixes0xF2
	legacyPrefixes0xF3
)

func (p legacyPrefixes) encode(c backend.Compiler) {
	switch p {
	case legacyPrefixesNone:
	case legacyPrefixes0x66:
		c.EmitByte(0x66)
	case legacyPrefixes0xF0:
		c.EmitByte(0xf0)
	case legacyPrefixes0x660xF0:
		c.EmitByte(0x66)
		c.EmitByte(0xf0)
	case legacyPrefixes0xF2:
		c.EmitByte(0xf2)
	case legacyPrefixes0xF3:
		c.EmitByte(0xf3)
	default:
		panic("BUG: invalid legacy prefix")
	}
}

func lower32willSignExtendTo64(x uint64) bool {
	xs := int64(x)
	return xs == int64(uint64(int32(xs)))
}

func lower8willSignExtendTo32(x uint32) bool {
	xs := int32(x)
	return xs == ((xs << 24) >> 24)
}

// newScratchInstr returns a zeroed instruction to build the pieces of a
// pseudo-instruction expansion at encoding time. machine.allocateInstr always
// hands out a reset instruction (see resetInstruction), so the asXxx
// constructors are allowed to leave the fields they do not set alone, and a
// fresh one per piece is what keeps that assumption true here too.
func newScratchInstr() *instruction {
	return &instruction{}
}

// emitJmpRel32 emits `jmp rel32`, which is `E9 cd` (Intel SDM Vol.2A, JMP),
// with a placeholder displacement to be filled in by bindRel32. The returned
// offset is the one just past the displacement, which is both where bindRel32
// writes and what the displacement is relative to, since RIP points at the
// following instruction.
func emitJmpRel32(c backend.Compiler) int {
	newScratchInstr().asJmp(newOperandImm32(0)).encode(c)
	return len(c.Buf())
}

// emitJccRel32 emits `jcc rel32`, which is `0F 80+cc cd` (Intel SDM Vol.2A,
// Jcc), with a placeholder displacement to be filled in by bindRel32. See
// emitJmpRel32 for the returned offset.
func emitJccRel32(c backend.Compiler, cc cond) int {
	newScratchInstr().asJmpIf(cc, newOperandImm32(0)).encode(c)
	return len(c.Buf())
}

// bindRel32 patches the placeholder displacement recorded by emitJmpRel32 or
// emitJccRel32 so that the branch lands on the current end of the buffer.
func bindRel32(c backend.Compiler, at int) {
	buf := c.Buf()
	binary.LittleEndian.PutUint32(buf[at-4:], uint32(int32(len(buf)-at)))
}

// emitIconst materializes v in the integer register dst, mirroring
// machine.lowerIconst: zero is cheaper to produce with a xor than with a mov.
func emitIconst(c backend.Compiler, dst regalloc.VReg, v uint64, _64 bool) {
	if v == 0 {
		newScratchInstr().asZeros(dst).encode(c)
		return
	}
	newScratchInstr().asImm(dst, v, _64).encode(c)
}

// emitExitWithCode emits the machine code of machine.lowerExitWithCode: it
// records the stack pointer, frame pointer and exit code in the execution
// context, saves the address of the trapping site so the Go side can unwind,
// and then leaves through the exit sequence. Control never comes back, so
// whatever is emitted next is only reachable by a branch around this.
//
// The amode is a plain local because encodeRegMem takes it as a real pointer.
// It must not be handed to newOperandMem, which launders the pointer through a
// uintptr and so needs the pooled, heap-allocated amodes machine.amodePool
// keeps alive.
func emitExitWithCode(c backend.Compiler, execCtx regalloc.VReg, code nativeapi.ExitCode) {
	am := amode{kindWithShift: uint32(amodeImmReg), base: execCtx}

	// Save the stack and frame pointers the Go side has to return to. Both are
	// `MOV r/m64, r64`, that is `REX.W + 89 /r` (Intel SDM Vol.2B, MOV), the
	// same encoding movRM uses for its 8-byte case.
	am.imm32 = nativeapi.ExecutionContextOffsetStackPointerBeforeGoCall.U32()
	encodeRegMem(c, legacyPrefixesNone, 0x89, 1, regEncodings[rsp], &am, rexInfo(0).setW())
	am.imm32 = nativeapi.ExecutionContextOffsetFramePointerBeforeGoCall.U32()
	encodeRegMem(c, legacyPrefixesNone, 0x89, 1, regEncodings[rbp], &am, rexInfo(0).setW())

	// RBP has been saved, so it is free to carry the exit code. The store is
	// 4-byte, `MOV r/m32, r32` = `89 /r`.
	newScratchInstr().asImm(rbpVReg, uint64(code), false).encode(c)
	am.imm32 = nativeapi.ExecutionContextOffsetExitCodeOffset.U32()
	encodeRegMem(c, legacyPrefixesNone, 0x89, 1, regEncodings[rbp], &am, rexInfo(0).clearW())

	// Save the address of this site for stack unwinding. `lea disp32(%rip), %rbp`
	// is `REX.W + 8D /r` (Intel SDM Vol.2A, LEA) with ModRM.mod=00 and
	// ModRM.rm=101, the RIP+disp32 form in 64-bit mode. RBP needs no REX.R, so
	// the instruction is exactly 7 bytes (REX + opcode + ModRM + disp32) and a
	// displacement of -7 names its own first byte. That is the very address
	// machine.lowerExitWithCode records, since it puts the label immediately
	// before the LEA.
	const leaSize = 7
	rbpEnc := regEncodings[rbp]
	rexInfo(0).setW().encode(c, regRexBit(byte(rbpEnc)), 0)
	c.EmitByte(0x8d)
	c.EmitByte(encodeModRM(0b00, rbpEnc.encoding(), 0b101))
	selfDisp := int32(-leaSize)
	c.Emit4Bytes(uint32(selfDisp))

	am.imm32 = nativeapi.ExecutionContextOffsetGoCallReturnAddress.U32()
	encodeRegMem(c, legacyPrefixesNone, 0x89, 1, regEncodings[rbp], &am, rexInfo(0).setW())

	// Restore RBP and RSP from the execution context and return to the Go
	// world, exactly as the exitSequence case above does.
	am.imm32 = nativeapi.ExecutionContextOffsetOriginalFramePointer.U32()
	encodeLoad64(c, &am, rbp)
	am.imm32 = nativeapi.ExecutionContextOffsetOriginalStackPointer.U32()
	encodeLoad64(c, &am, rsp)
	encodeRet(c)
}
