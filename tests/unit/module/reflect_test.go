package module_test

import "reflect"

// newOf returns a pointer to a new zero value of the same type as v.
func newOf(v any) any { return reflect.New(reflect.TypeOf(v)).Interface() }

// deref returns the value a pointer made by newOf points to.
func deref(p any) any { return reflect.ValueOf(p).Elem().Interface() }
