package service

import (
	"context"
	"strings"
	"testing"

	"github.com/frankbardon/aperture/csvprovider"
	"github.com/frankbardon/aperture/engine"
	aerr "github.com/frankbardon/aperture/errors"
	"github.com/frankbardon/aperture/provider"
	"github.com/frankbardon/aperture/storage/memory"
)

func newBrandRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	p, err := csvprovider.FromReader(strings.NewReader(
		"id,category_id\nbrand:1,category:1\nbrand:2,category:1\nbrand:3,category:2\n"))
	if err != nil {
		t.Fatalf("FromReader: %v", err)
	}
	reg := provider.NewRegistry()
	reg.MustRegister("brand", p)
	return reg
}

func TestObjectIdentifiers(t *testing.T) {
	svc := New(engine.New(memory.New()), WithProviders(newBrandRegistry(t)))

	all, err := svc.ObjectIdentifiers(context.Background(), "brand")
	if err != nil {
		t.Fatalf("ObjectIdentifiers: %v", err)
	}
	if got := strings.Join(all, ","); got != "brand:1,brand:2,brand:3" {
		t.Fatalf("all = %q, want brand:1,brand:2,brand:3", got)
	}

	// Exclusive expansion: all EXCEPT brand:2.
	pos, err := svc.ObjectIdentifiers(context.Background(), "brand", "brand:2")
	if err != nil {
		t.Fatalf("ObjectIdentifiers exclude: %v", err)
	}
	if got := strings.Join(pos, ","); got != "brand:1,brand:3" {
		t.Fatalf("positive list = %q, want brand:1,brand:3", got)
	}
}

func TestObjectIdentifiers_Unwired(t *testing.T) {
	svc := New(engine.New(memory.New())) // no WithProviders
	_, err := svc.ObjectIdentifiers(context.Background(), "brand")
	if aerr.CodeOf(err) != aerr.APERTURE_UNIMPLEMENTED {
		t.Fatalf("code = %s, want APERTURE_UNIMPLEMENTED", aerr.CodeOf(err))
	}
}

func TestObjectIdentifiers_UnregisteredType(t *testing.T) {
	svc := New(engine.New(memory.New()), WithProviders(newBrandRegistry(t)))
	_, err := svc.ObjectIdentifiers(context.Background(), "app")
	if aerr.CodeOf(err) != aerr.APERTURE_PROVIDER_UNREGISTERED {
		t.Fatalf("code = %s, want APERTURE_PROVIDER_UNREGISTERED", aerr.CodeOf(err))
	}
}

func TestObjectIdentifiers_BadExcludeID(t *testing.T) {
	svc := New(engine.New(memory.New()), WithProviders(newBrandRegistry(t)))
	_, err := svc.ObjectIdentifiers(context.Background(), "brand", "not a valid id")
	if aerr.CodeOf(err) != aerr.APERTURE_IDENTITY_INVALID {
		t.Fatalf("code = %s, want APERTURE_IDENTITY_INVALID", aerr.CodeOf(err))
	}
}
