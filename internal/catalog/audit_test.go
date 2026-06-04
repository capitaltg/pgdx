package catalog

import (
	"strings"
	"testing"
)

func TestSuperuserRolesQuery(t *testing.T) {
	q := superuserRolesQuery()
	for _, want := range []string{"rolcanlogin", "rolsuper", "rolbypassrls", "r.oid = 10", "!~ '^pg_'"} {
		if !strings.Contains(q, want) {
			t.Errorf("superuserRolesQuery missing %q:\n%s", want, q)
		}
	}
}

func TestClassifySuperuser(t *testing.T) {
	tests := []struct {
		name      string
		bootstrap bool
		wantLabel string
		wantExp   bool
	}{
		{"rdsadmin", true, "rdsadmin (AWS RDS-managed)", true}, // RDS: provider role IS the bootstrap
		{"cloudsqladmin", true, "cloudsqladmin (GCP Cloud SQL-managed)", true},
		{"azuresu", true, "azuresu (Azure-managed)", true},
		{"postgres", true, "postgres (bootstrap)", true},        // self-managed bootstrap
		{"appadmin", false, "appadmin", false},                  // user-created — the actionable case
		{"rdsadmin", false, "rdsadmin (AWS RDS-managed)", true}, // provider role even if not bootstrap-flagged
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			label, exp := classifySuperuser(tt.name, tt.bootstrap)
			if label != tt.wantLabel || exp != tt.wantExp {
				t.Errorf("classifySuperuser(%q,%v) = (%q,%v), want (%q,%v)",
					tt.name, tt.bootstrap, label, exp, tt.wantLabel, tt.wantExp)
			}
		})
	}
}

func TestPublicSchemaQuery(t *testing.T) {
	q := publicSchemaQuery()
	for _, want := range []string{"aclexplode", "a.grantee = 0", "nspname = 'public'", "nspacl IS NULL"} {
		if !strings.Contains(q, want) {
			t.Errorf("publicSchemaQuery missing %q:\n%s", want, q)
		}
	}
}

func TestPublicSchemaFinding(t *testing.T) {
	tests := []struct {
		name         string
		aclDefault   bool
		publicCreate bool
		version      int
		wantFinding  bool
	}{
		{"explicit public CREATE grant", false, true, 160000, true},
		{"pre-15 default ACL is open", true, false, 140000, true},
		{"pg15+ default ACL is safe", true, false, 150000, false},
		{"pg16 default ACL is safe", true, false, 160000, false},
		{"locked-down explicit ACL", false, false, 160000, false},
		{"unknown version, no positive grant", true, false, 0, false},
		{"unknown version, positive grant still flags", false, true, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := publicSchemaFinding(tt.aclDefault, tt.publicCreate, tt.version)
			if tt.wantFinding != (f != nil) {
				t.Fatalf("publicSchemaFinding(%v,%v,%d) finding=%v, want %v",
					tt.aclDefault, tt.publicCreate, tt.version, f != nil, tt.wantFinding)
			}
			if f != nil && f.Severity != SeverityWarning {
				t.Errorf("public schema finding should be a warning, got %q", f.Severity)
			}
		})
	}
}

func TestRLSDisabledQuery(t *testing.T) {
	q := rlsDisabledQuery()
	for _, want := range []string{"NOT c.relrowsecurity", "pg_policy", "relkind IN ('r','p')"} {
		if !strings.Contains(q, want) {
			t.Errorf("rlsDisabledQuery missing %q:\n%s", want, q)
		}
	}
}

func TestHBAAuthQuery(t *testing.T) {
	q := hbaAuthQuery()
	for _, want := range []string{"pg_hba_file_rules", "auth_method IN ('trust', 'password', 'md5')", "error IS NULL", "array_to_string"} {
		if !strings.Contains(q, want) {
			t.Errorf("hbaAuthQuery missing %q:\n%s", want, q)
		}
	}
}

func TestClassifyHBAAuth(t *testing.T) {
	tests := []struct {
		method string
		want   hbaAuthCategory
	}{
		{"trust", hbaTrust},
		{"password", hbaCleartext},
		{"md5", hbaWeakHash},
		{"scram-sha-256", hbaSecure},
		{"cert", hbaSecure},
		{"gss", hbaSecure},
	}
	for _, tt := range tests {
		if got := classifyHBAAuth(tt.method); got != tt.want {
			t.Errorf("classifyHBAAuth(%q) = %q, want %q", tt.method, got, tt.want)
		}
	}
}

func TestPrivilegedRolesQuery(t *testing.T) {
	q := privilegedRolesQuery()
	for _, want := range []string{"pg_execute_server_program", "pg_read_all_data", "pg_has_role", "NOT r.rolsuper", "'MEMBER'"} {
		if !strings.Contains(q, want) {
			t.Errorf("privilegedRolesQuery missing %q:\n%s", want, q)
		}
	}
}

func TestClassifyPrivilegedRoles(t *testing.T) {
	in := map[string][]string{
		"shell":     {"pg_execute_server_program"},
		"reader":    {"pg_read_all_data"},
		"both":      {"pg_execute_server_program", "pg_read_all_data"}, // RCE wins; not double-listed
		"filewrite": {"pg_write_server_files"},
	}
	rce, data := classifyPrivilegedRoles(in)

	rceStr := strings.Join(rce, " | ")
	for _, want := range []string{"shell (pg_execute_server_program)", "both (pg_execute_server_program)", "filewrite (pg_write_server_files)"} {
		if !strings.Contains(rceStr, want) {
			t.Errorf("rce list missing %q: %v", want, rce)
		}
	}
	dataStr := strings.Join(data, " | ")
	if !strings.Contains(dataStr, "reader (pg_read_all_data)") {
		t.Errorf("data list should include reader: %v", data)
	}
	if strings.Contains(dataStr, "both") {
		t.Errorf("a role with RCE access must not also appear in the data list: %v", data)
	}
}

func TestSchemaPublicCreateQuery(t *testing.T) {
	q := schemaPublicCreateQuery()
	for _, want := range []string{"aclexplode", "a.grantee = 0", "privilege_type = 'CREATE'", "nspname <> 'public'"} {
		if !strings.Contains(q, want) {
			t.Errorf("schemaPublicCreateQuery missing %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "USAGE") {
		t.Errorf("schemaPublicCreateQuery must NOT flag USAGE (too noisy):\n%s", q)
	}
}

func TestSessionSSLFinding(t *testing.T) {
	tests := []struct {
		name      string
		ssl       bool
		localSock bool
		loopback  bool
		wantNil   bool
		wantSev   Severity
	}{
		{"encrypted", true, false, false, true, ""},
		{"unix socket", false, true, false, true, ""},
		{"loopback cleartext", false, false, true, false, SeverityInfo},
		{"remote cleartext", false, false, false, false, SeverityWarning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := sessionSSLFinding(tt.ssl, tt.localSock, tt.loopback, "10.0.0.5")
			if tt.wantNil != (f == nil) {
				t.Fatalf("sessionSSLFinding nil=%v, want nil=%v", f == nil, tt.wantNil)
			}
			if f != nil && f.Severity != tt.wantSev {
				t.Errorf("severity = %q, want %q", f.Severity, tt.wantSev)
			}
		})
	}
}

func severitiesByCheck(findings []SecurityFinding) map[string]Severity {
	m := map[string]Severity{}
	for _, f := range findings {
		m[f.Title] = f.Severity
	}
	return m
}

func TestDurabilityFindings(t *testing.T) {
	t.Run("all safe → no findings", func(t *testing.T) {
		if f := durabilityFindings("on", "on", "off", "on"); len(f) != 0 {
			t.Fatalf("want no findings for safe settings, got %v", f)
		}
	})
	t.Run("fsync and full_page_writes off are critical", func(t *testing.T) {
		got := severitiesByCheck(durabilityFindings("off", "off", "off", "on"))
		if got["fsync is off"] != SeverityCritical || got["full_page_writes is off"] != SeverityCritical {
			t.Fatalf("expected criticals, got %v", got)
		}
	})
	t.Run("zero_damaged_pages on is a warning", func(t *testing.T) {
		got := severitiesByCheck(durabilityFindings("on", "on", "on", "on"))
		if got["zero_damaged_pages is on"] != SeverityWarning {
			t.Fatalf("expected warning, got %v", got)
		}
	})
	t.Run("synchronous_commit off is info, not a warning", func(t *testing.T) {
		got := severitiesByCheck(durabilityFindings("on", "on", "off", "off"))
		if got["synchronous_commit is off"] != SeverityInfo {
			t.Fatalf("expected info, got %v", got)
		}
	})
}

func TestAutovacuumFindings(t *testing.T) {
	if f := autovacuumFindings("on", "on"); len(f) != 0 {
		t.Fatalf("want no findings when healthy, got %v", f)
	}
	got := severitiesByCheck(autovacuumFindings("off", "off"))
	if got["autovacuum is disabled"] != SeverityCritical {
		t.Fatalf("autovacuum off should be critical, got %v", got)
	}
	if got["track_counts is off"] != SeverityWarning {
		t.Fatalf("track_counts off should be warning, got %v", got)
	}
}

func TestCheckpointPressureFinding(t *testing.T) {
	t.Run("too few checkpoints → nil", func(t *testing.T) {
		if f := checkpointPressureFinding(CheckpointStats{Timed: 2, Requested: 3}); f != nil {
			t.Fatalf("want nil for tiny sample, got %v", f)
		}
	})
	t.Run("mostly timed → nil", func(t *testing.T) {
		if f := checkpointPressureFinding(CheckpointStats{Timed: 100, Requested: 5}); f != nil {
			t.Fatalf("want nil when healthy, got %v", f)
		}
	})
	t.Run("mostly forced → warning", func(t *testing.T) {
		f := checkpointPressureFinding(CheckpointStats{Timed: 10, Requested: 40})
		if f == nil || f.Severity != SeverityWarning {
			t.Fatalf("expected a warning finding, got %v", f)
		}
	})
}

func TestReliabBoolOff(t *testing.T) {
	for _, off := range []string{"off", "false", "0", "no", "", " OFF "} {
		if !reliabBoolOff(off) {
			t.Errorf("reliabBoolOff(%q) = false, want true", off)
		}
	}
	for _, on := range []string{"on", "true", "1", "local"} {
		if reliabBoolOff(on) {
			t.Errorf("reliabBoolOff(%q) = true, want false", on)
		}
	}
}

func TestIsLogOn(t *testing.T) {
	for _, on := range []string{"on", "true", "1", "all", "ON"} {
		if !isLogOn(on) {
			t.Errorf("isLogOn(%q) = false, want true", on)
		}
	}
	for _, off := range []string{"off", "false", "0", "", "  off  "} {
		if isLogOn(off) {
			t.Errorf("isLogOn(%q) = true, want false", off)
		}
	}
}

func TestUntrustedLanguagesQuery(t *testing.T) {
	q := untrustedLanguagesQuery()
	if !strings.Contains(q, "lanispl") || !strings.Contains(q, "NOT lanpltrusted") {
		t.Errorf("untrustedLanguagesQuery should filter installed untrusted languages:\n%s", q)
	}
}

func TestSeverityRank(t *testing.T) {
	if SeverityCritical.rank() >= SeverityWarning.rank() || SeverityWarning.rank() >= SeverityInfo.rank() {
		t.Fatal("severity rank must order critical < warning < info")
	}
}

func TestAuditCountsAndThreshold(t *testing.T) {
	a := &SecurityAudit{Findings: []SecurityFinding{
		{Severity: SeverityCritical},
		{Severity: SeverityWarning},
		{Severity: SeverityWarning},
		{Severity: SeverityInfo},
	}}
	crit, warn, info := a.Counts()
	if crit != 1 || warn != 2 || info != 1 {
		t.Fatalf("Counts() = (%d,%d,%d), want (1,2,1)", crit, warn, info)
	}
	if !a.HasAtLeast(SeverityWarning) || !a.HasAtLeast(SeverityCritical) {
		t.Error("audit with a critical finding should satisfy HasAtLeast(warning) and (critical)")
	}

	infoOnly := &SecurityAudit{Findings: []SecurityFinding{{Severity: SeverityInfo}}}
	if infoOnly.HasAtLeast(SeverityWarning) {
		t.Error("info-only audit must not satisfy HasAtLeast(warning)")
	}
	clean := &SecurityAudit{}
	if clean.HasAtLeast(SeverityInfo) {
		t.Error("clean audit must not satisfy HasAtLeast(info)")
	}
}
