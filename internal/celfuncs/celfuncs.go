// Package celfuncs provides the deterministic, side-effect free CEL helper
// functions that greenlight rules can use to derive values such as prefixes and
// IP addresses.
//
// All functions take and return plain strings (not the Kubernetes `ip` / `cidr`
// CEL types) so that rules stay simple, and all of them support IPv4 and IPv6.
// Allocation is hash based and therefore deterministic: the same key always
// yields the same value, across restarts and across delete/recreate cycles.
package celfuncs

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// Library returns the CEL environment option that declares all greenlight
// helper functions.
func Library() cel.EnvOption {
	return cel.Lib(lib{})
}

type lib struct{}

func (lib) LibraryName() string { return "greenlight.celfuncs" }

func (lib) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("hash",
			cel.Overload("greenlight_hash_string_int",
				[]*cel.Type{cel.StringType}, cel.IntType,
				cel.UnaryBinding(func(key ref.Val) ref.Val {
					s, ok := key.(types.String)
					if !ok {
						return types.MaybeNoSuchOverloadErr(key)
					}
					return types.Int(Hash(string(s)))
				}),
			),
		),
		cel.Function("cidrSubnet",
			cel.Overload("greenlight_cidrSubnet_string_int_int_string",
				[]*cel.Type{cel.StringType, cel.IntType, cel.IntType}, cel.StringType,
				cel.FunctionBinding(func(args ...ref.Val) ref.Val {
					parent, err := stringArg(args[0])
					if err != nil {
						return err
					}
					length, err := intArg(args[1])
					if err != nil {
						return err
					}
					index, err := intArg(args[2])
					if err != nil {
						return err
					}
					return str(CIDRSubnet(parent, int(length), index))
				}),
			),
		),
		cel.Function("cidrHost",
			cel.Overload("greenlight_cidrHost_string_int_string",
				[]*cel.Type{cel.StringType, cel.IntType}, cel.StringType,
				cel.BinaryBinding(func(prefix, index ref.Val) ref.Val {
					p, err := stringArg(prefix)
					if err != nil {
						return err
					}
					i, err := intArg(index)
					if err != nil {
						return err
					}
					return str(CIDRHost(p, i))
				}),
			),
		),
		cel.Function("pickSubnet",
			cel.Overload("greenlight_pickSubnet_string_int_string_string",
				[]*cel.Type{cel.StringType, cel.IntType, cel.StringType}, cel.StringType,
				cel.FunctionBinding(func(args ...ref.Val) ref.Val {
					parent, err := stringArg(args[0])
					if err != nil {
						return err
					}
					length, err := intArg(args[1])
					if err != nil {
						return err
					}
					key, err := stringArg(args[2])
					if err != nil {
						return err
					}
					return str(PickSubnet(parent, int(length), key))
				}),
			),
		),
		cel.Function("pickHost",
			cel.Overload("greenlight_pickHost_string_string_string",
				[]*cel.Type{cel.StringType, cel.StringType}, cel.StringType,
				cel.BinaryBinding(func(prefix, key ref.Val) ref.Val {
					p, err := stringArg(prefix)
					if err != nil {
						return err
					}
					k, err := stringArg(key)
					if err != nil {
						return err
					}
					return str(PickHost(p, k))
				}),
			),
		),
		cel.Function("hostCIDR",
			cel.Overload("greenlight_hostCIDR_string_string",
				[]*cel.Type{cel.StringType}, cel.StringType,
				cel.UnaryBinding(func(addr ref.Val) ref.Val {
					a, err := stringArg(addr)
					if err != nil {
						return err
					}
					return str(HostCIDR(a))
				}),
			),
		),
	}
}

func (lib) ProgramOptions() []cel.ProgramOption { return nil }

func stringArg(v ref.Val) (string, ref.Val) {
	s, ok := v.(types.String)
	if !ok {
		return "", types.MaybeNoSuchOverloadErr(v)
	}
	return string(s), nil
}

func intArg(v ref.Val) (int64, ref.Val) {
	i, ok := v.(types.Int)
	if !ok {
		return 0, types.MaybeNoSuchOverloadErr(v)
	}
	return int64(i), nil
}

func str(s string, err error) ref.Val {
	if err != nil {
		return types.NewErr("%s", err.Error())
	}
	return types.String(s)
}
