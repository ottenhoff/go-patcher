package main

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
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
func TestUnrollTarball(t *testing.T) {
	// Create directory structure and file with XML content
	err := os.MkdirAll("components/sakai-provider-pack/WEB-INF", 0755)
	if err != nil {
		t.Fatalf("Failed to create test directory structure: %v", err)
	}

	xmlContent := `<?xml version="1.0" encoding="UTF-8"?>
<beans>
    <bean id="myBean" class="com.example.MyClass">
        <property name="someProperty" value="someValue"/>
    </bean>
</beans>`

	err = os.WriteFile("components/sakai-provider-pack/WEB-INF/unboundid-ldap.xml", []byte(xmlContent), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// Call the function under test
	result := unrollTarball("test.tar.gz")

	// Verify the result
	expected := map[string]int{
		"components/sakai-provider-pack": 4,
	}
	if !reflect.DeepEqual(result, expected) {
		t.Errorf("unrollTarball() returned unexpected result. Got: %v, Want: %v", result, expected)
	}

	// Verify the XML file still exists and contains our content
	content, err := os.ReadFile("components/sakai-provider-pack/WEB-INF/unboundid-ldap.xml")
	if err != nil {
		t.Errorf("Failed to read XML file: %v", err)
	}
	if string(content) != xmlContent {
		t.Errorf("XML file was overwritten. Got: %s, Want: %s", string(content), xmlContent)
	}

	assert.True(t, pathExists("components/sakai-provider-pack/WEB-INF/components.txt"))
	// No file existed so it should be created
	assert.True(t, pathExists("components/sakai-provider-pack/WEB-INF/components.xml"))
}

func TestModifyPropertyFiles(t *testing.T) {
	// Setup test directory structure
	tmpDir := t.TempDir()
	os.MkdirAll(tmpDir+"/sakai", 0755)
	t.Logf("Test directory: %s", tmpDir)

	// Copy test files from testdata directory
	testFiles := []string{
		"testdata/sakai.properties",
		"testdata/local.properties",
	}

	for _, file := range testFiles {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("Failed to read test file %s: %v", file, err)
		}
		destPath := tmpDir + "/sakai/" + filepath.Base(file)
		err = os.WriteFile(destPath, content, 0644)
		if err != nil {
			t.Fatalf("Failed to write test file %s: %v", destPath, err)
		}
	}

	// Change working directory for test
	originalWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(originalWd)

	// Test property modification
	patchID := "63547"
	modifyPropertyFiles("portal.cdn.version="+patchID[len(patchID)-3:], patchID)

	// Verify modifications in sakai.properties
	sakaiContent, _ := os.ReadFile("sakai/sakai.properties")
	assert.Contains(t, string(sakaiContent), "#portal.cdn.version=")
	assert.Contains(t, string(sakaiContent), "portal.cdn.version=547")

	// Verify local.properties is unchanged
	localContent, _ := os.ReadFile("sakai/local.properties")
	assert.Contains(t, string(localContent), "smtp.test=java")
	assert.NotContains(t, string(localContent), "portal.cdn.version")
}
