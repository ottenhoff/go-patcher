package main

import (
	"flag"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	flag.Set("token", "your-test-token")
	os.Exit(m.Run())
}

func TestReplaceNumbers(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{"JAXB Impl", "jaxb-impl-2.3.3.jar", "jaxb-impl-*.*.*.jar"},
		{"Ignite Hibernate Core", "ignite-hibernate-core-2.12.0.jar", "ignite-hibernate-core-*.*.*.jar"},
		{"Commons Text 1.9", "commons-text-1.9.jar", "commons-text-*.*.jar"},
		{"Commons Text 1.11", "commons-text-1.11.0.jar", "commons-text-*.*.*.jar"},
		{"Spring Expression", "spring-expression-5.3.18.jar", "spring-expression-*.*.*.jar"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := replaceNumbers(tc.input)
			if actual != tc.expected {
				t.Errorf("replaceNumbers(%q) = %q; want %q", tc.input, actual, tc.expected)
			}
		})
	}
}
func TestPathExists(t *testing.T) {
	existingPath := "README.md"
	nonExistingPath := "BLANK.md"

	existing := pathExists(existingPath)
	if !existing {
		t.Errorf("pathExists(%q) = false; want true", existingPath)
	}

	nonExisting := pathExists(nonExistingPath)
	if nonExisting {
		t.Errorf("pathExists(%q) = true; want false", nonExistingPath)
	}
}
func TestCheckTomcatDirExists(t *testing.T) {
	existingDir := "/tmp"
	checkTomcatDirExists(existingDir)
}

func TestShouldSkipFile(t *testing.T) {
	tests := []struct {
		filename string
		want     bool
	}{
		{"components/sakai-provider-pack/WEB-INF/lib/unboundid-ldapsdk-6.0.1.jar", false},
		{"components/sakai-provider-pack/META-INF/MANIFEST.MF", false},
		{"components/sakai-provider-pack/WEB-INF/unboundid-ldap.xml", true},
		{"components/sakai-provider-pack/WEB-INF/unboundid-ldap-2.xml", true},
		{"components/sakai-provider-pack/WEB-INF/jldap-beans.xml", true},
		{"components/sakai-provider-pack/WEB-INF/jldap-beans-1.xml", true},
		{"components/sakai-provider-pack/WEB-INF/jldap-beans-mic.xml", true},
		{"components/sakai-provider-pack/WEB-INF/components.xml", true},
		{"components/elasticsearch-impl/WEB-INF/lib/entitybroker-utils-22-SNAPSHOT.jar", false},
		// Updated test cases with long paths
		{"path/to/unbounded-ldap.xml", false},
		{"path/to/unboundid-ldap-1.xml", false},
		{"another/path/unbounded-ldap-2.xml", false},
		{"yet/another/path/unboundid-ldap-faculty.xml", false},
		{"some/random/path/components.xml", false},
		{"different/path/jldap-beans.xml", false},
		{"unique/path/jldap-beans-123.xml", false},
		{"varied/path/jldap-beans-department.xml", false},
		// Additional test cases to ensure robust matching
		{"miscellaneous/path/other-file.xml", false},
		{"another/directory/unboundid.xml", false},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			if got := shouldSkipFile(tt.filename); got != tt.want {
				t.Errorf("shouldSkipFile(%q) = %v, want %v", tt.filename, got, tt.want)
			}
		})
	}
}
