package archtest

import "testing"

func TestForeignSchemaIsReported(t *testing.T) {
	v, err := ForeignSchemaRefs("testdata/schema-foreign")
	if err != nil || len(v) != 1 || v[0].Module != "gateway" || v[0].Schema != "identity" {
		t.Fatalf("got %+v err=%v", v, err)
	}
}

func TestOwnAndPlatformSchemasAreAllowed(t *testing.T) {
	v, err := ForeignSchemaRefs("testdata/schema-allowed")
	if err != nil || len(v) != 0 {
		t.Fatalf("got %+v err=%v", v, err)
	}
}

func TestRepositoryHasNoForeignSchemaRefs(t *testing.T) {
	v, err := ForeignSchemaRefs("../modules")
	if err != nil || len(v) != 0 {
		t.Fatalf("got %+v err=%v", v, err)
	}
}

func TestImpureStdlibInDomainIsReported(t *testing.T) {
	v, err := ImpureDomainImports("testdata/domain-stdlib")
	if err != nil || len(v) != 1 || v[0].Module != "catalog" || v[0].Import != "net/http" {
		t.Fatalf("got %+v err=%v", v, err)
	}
}

func TestPureStdlibInDomainIsAllowed(t *testing.T) {
	v, err := ImpureDomainImports("testdata/domain-stdlib-allowed")
	if err != nil || len(v) != 0 {
		t.Fatalf("got %+v err=%v", v, err)
	}
}

func TestRepositoryDomainsAreStdlibPure(t *testing.T) {
	v, err := ImpureDomainImports("../modules")
	if err != nil || len(v) != 0 {
		t.Fatalf("got %+v err=%v", v, err)
	}
}

func TestEventNamesAreNotSchemaReferences(t *testing.T) {
	v, err := ForeignSchemaRefs("testdata/schema-events")
	if err != nil || len(v) != 0 {
		t.Fatalf("got %+v err=%v", v, err)
	}
}

func TestIoutilInDomainIsReported(t *testing.T) {
	v, err := ImpureDomainImports("testdata/domain-stdlib-ioutil")
	if err != nil || len(v) != 1 || v[0].Import != "io/ioutil" {
		t.Fatalf("got %+v err=%v", v, err)
	}
}
