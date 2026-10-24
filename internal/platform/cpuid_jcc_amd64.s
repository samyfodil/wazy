// Copyright 2024 The wazy Authors. Licensed under the Apache License 2.0.

//go:build amd64 && !tinygo

#include "textflag.h"

// func cpuid(eaxArg, ecxArg uint32) (eax, ebx, ecx, edx uint32)
//
// golang.org/x/sys/cpu runs CPUID but keeps the raw registers to itself and
// exposes only feature booleans; erratum SKX102 is keyed on family/model, which
// no exported API surfaces, so we read leaves 0 and 1 ourselves.
TEXT ·cpuid(SB), NOSPLIT, $0-24
	MOVL eaxArg+0(FP), AX
	MOVL ecxArg+4(FP), CX
	CPUID
	MOVL AX, eax+8(FP)
	MOVL BX, ebx+12(FP)
	MOVL CX, ecx+16(FP)
	MOVL DX, edx+20(FP)
	RET
