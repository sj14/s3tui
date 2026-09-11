package ui

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// withEditor replaces $EDITOR by a function which edits the file in place.
func withEditor(t *testing.T, edit func(path string) error) {
	t.Helper()

	original := editorHook
	t.Cleanup(func() { editorHook = original })

	t.Setenv("EDITOR", "true") // never runs, the hook below takes over

	editorHook = func(command *exec.Cmd, done tea.ExecCallback) tea.Cmd {
		return func() tea.Msg {
			return done(edit(command.Args[len(command.Args)-1]))
		}
	}
}

// writing returns an editor which replaces the whole document.
func writing(document string) func(string) error {
	return func(path string) error {
		return os.WriteFile(path, []byte(document), 0o600)
	}
}

// openConfig opens one of the configuration views of bucket-a.
func openConfig(t *testing.T, server *fakeServer, kind configKind) tea.Model {
	t.Helper()

	return openConfigSection(t, openBucketMenu(t, server), kind)
}

func TestBucketPolicyView(t *testing.T) {
	server := fakeS3(t)
	model := openConfig(t, server, configPolicy)

	got := model.(Model)
	if got.view != viewConfig || got.configKind != configPolicy {
		t.Fatalf("view = %v/%v, want the policy view", got.view, got.configKind)
	}
	if got.target != (configTarget{bucket: "bucket-a"}) {
		t.Errorf("target = %+v", got.target)
	}

	// the content scrolls, so assert on the whole rendered document
	view := model.View() + got.detailsContent

	for _, want := range []string{
		"bucket-a", "[policy]", "bucket policy", `"Sid": "PublicRead"`, `"s3:GetObject"`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the policy view misses %q:\n%s", want, view)
		}
	}

	// the lifecycle has its own view and does not leak into this one
	if strings.Contains(view, "expire-logs") {
		t.Errorf("the policy view shows lifecycle rules:\n%s", view)
	}

	// esc returns to the bucket overview
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewBucketMenu {
		t.Errorf("view = %v, want the bucket overview", got)
	}
}

func TestBucketLifecycleView(t *testing.T) {
	server := fakeS3(t)
	model := openConfig(t, server, configLifecycle)

	got := model.(Model)
	if got.view != viewConfig || got.configKind != configLifecycle {
		t.Fatalf("view = %v/%v, want the lifecycle view", got.view, got.configKind)
	}

	view := model.View() + got.detailsContent

	for _, want := range []string{
		"bucket-a", "[lifecycle]", "lifecycle configuration",
		`"ID": "expire-logs"`, `"Status": "Enabled"`, `"Prefix": "logs/"`, `"Days": 30`,
		`"NoncurrentDays": 7`, `"NewerNoncurrentVersions": 3`, `"DaysAfterInitiation": 7`,
		`"ID": "archive-raw"`, `"Status": "Disabled"`, `"Value": "cold"`, `"StorageClass": "GLACIER"`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the lifecycle view misses %q:\n%s", want, view)
		}
	}

	if strings.Contains(view, "PublicRead") {
		t.Errorf("the lifecycle view shows the policy:\n%s", view)
	}

	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewBucketMenu {
		t.Errorf("view = %v, want the bucket overview", got)
	}
}

func TestBucketWithoutAConfiguration(t *testing.T) {
	server := fakeS3(t)

	for _, test := range []struct {
		kind configKind
		want string
	}{
		{configPolicy, "no bucket policy"},
		{configLifecycle, "no lifecycle configuration"},
		{configPublicAccess, "no public access block"},
		{configCORS, "no CORS configuration"},
		{configObjectLock, "no object lock configuration"},
	} {
		model := newTestModel(t, server.URL)

		inner := model.(Model)
		inner.buckets.Select(1) // bucket-b, which has none of them
		model = openConfigSection(t, enter(t, tea.Model(inner)), test.kind)

		view := model.View() + model.(Model).detailsContent
		if !strings.Contains(view, test.want) {
			t.Errorf("the view misses %q:\n%s", test.want, view)
		}
		if strings.Contains(view, "error:") {
			t.Errorf("a missing configuration must not be an error:\n%s", view)
		}
	}
}

func TestConfigViewFooters(t *testing.T) {
	server := fakeS3(t)

	menu := openBucketMenu(t, server).View()
	for _, want := range []string{
		"objects", "delete markers", "multipart uploads",
		"policy", "lifecycle", "acl", "public access block", "cors", "versioning", "object lock",
	} {
		if !strings.Contains(menu, want) {
			t.Errorf("the bucket overview misses %q:\n%s", want, menu)
		}
	}

	for _, kind := range []configKind{configPolicy, configLifecycle, configPublicAccess, configCORS} {
		footer := openConfig(t, server, kind).View()
		for _, want := range []string{"e edit", "d delete", "esc back", "? help"} {
			if !strings.Contains(footer, want) {
				t.Errorf("the footer of %v misses %q:\n%s", kind, want, footer)
			}
		}
	}

	// what S3 cannot take off a bucket again is not offered
	for _, kind := range []configKind{configACL, configVersioning, configObjectLock} {
		footer := openConfig(t, server, kind).View()
		if !strings.Contains(footer, "e edit") {
			t.Errorf("the footer of %v misses the edit key:\n%s", kind, footer)
		}
		if strings.Contains(footer, "d delete") {
			t.Errorf("the footer of %v offers a delete which does not exist:\n%s", kind, footer)
		}
	}
}

func TestEditBucketPolicy(t *testing.T) {
	server := fakeS3(t)
	edited := `{"Version":"2012-10-17","Statement":[{"Sid":"DenyAll","Effect":"Deny","Principal":"*","Action":["s3:*"],"Resource":"arn:aws:s3:::bucket-a/*"}]}`

	withEditor(t, writing(edited))

	model := press(t, openConfig(t, server, configPolicy), 'e')

	stored, ok := server.config("policy", "bucket-a")
	if !ok || stored != edited {
		t.Fatalf("the bucket policy was not written, stored = %q", stored)
	}

	got := model.(Model)
	if got.err != nil {
		t.Errorf("unexpected error: %v", got.err)
	}
	if !strings.Contains(got.status, "saved") {
		t.Errorf("status = %q, want it to confirm the save", got.status)
	}

	// the view was reloaded, so it shows what the bucket holds now
	view := model.View() + got.detailsContent
	if !strings.Contains(view, `"Sid": "DenyAll"`) {
		t.Errorf("the view still shows the old policy:\n%s", view)
	}
}

func TestEditLifecycle(t *testing.T) {
	server := fakeS3(t)

	withEditor(t, writing(`{"Rules":[{"ID":"only-rule","Status":"Enabled","Filter":{"Prefix":"tmp/"},"Expiration":{"Days":1}}]}`))

	model := press(t, openConfig(t, server, configLifecycle), 'e')

	stored, ok := server.config("lifecycle", "bucket-a")
	if !ok {
		t.Fatal("the lifecycle configuration was removed instead of written")
	}
	for _, want := range []string{"<ID>only-rule</ID>", "<Prefix>tmp/</Prefix>", "<Days>1</Days>"} {
		if !strings.Contains(stored, want) {
			t.Errorf("the stored configuration misses %q:\n%s", want, stored)
		}
	}
	if strings.Contains(stored, "expire-logs") {
		t.Errorf("the old rules were kept:\n%s", stored)
	}

	got := model.(Model)
	if got.err != nil {
		t.Errorf("unexpected error: %v", got.err)
	}

	view := model.View() + got.detailsContent
	if !strings.Contains(view, `"ID": "only-rule"`) {
		t.Errorf("the view was not reloaded:\n%s", view)
	}
}

func TestEditWithoutChangesWritesNothing(t *testing.T) {
	server := fakeS3(t)
	before, _ := server.config("policy", "bucket-a")

	withEditor(t, func(string) error { return nil }) // an editor which was closed again

	model := press(t, openConfig(t, server, configPolicy), 'e')

	after, _ := server.config("policy", "bucket-a")
	if after != before {
		t.Errorf("the policy changed without an edit:\n%s", after)
	}
	if status := model.(Model).status; !strings.Contains(status, "unchanged") {
		t.Errorf("status = %q, want it to say that nothing was written", status)
	}
}

func TestEditRejectsABrokenDocument(t *testing.T) {
	server := fakeS3(t)
	before, _ := server.config("policy", "bucket-a")

	withEditor(t, writing("{ not json"))

	model := press(t, openConfig(t, server, configPolicy), 'e')

	if after, _ := server.config("policy", "bucket-a"); after != before {
		t.Errorf("a broken document was written:\n%s", after)
	}

	got := model.(Model)
	if got.err == nil || !strings.Contains(got.err.Error(), "valid JSON") {
		t.Errorf("err = %v, want a complaint about the JSON", got.err)
	}
}

func TestEditRejectsAnEmptyDocument(t *testing.T) {
	server := fakeS3(t)

	withEditor(t, writing("   \n"))

	model := press(t, openConfig(t, server, configLifecycle), 'e')

	if _, ok := server.config("lifecycle", "bucket-a"); !ok {
		t.Error("an emptied document deleted the configuration, d is what deletes")
	}

	got := model.(Model)
	if got.err == nil || !strings.Contains(got.err.Error(), "d deletes it") {
		t.Errorf("err = %v, want the hint that d deletes", got.err)
	}
}

func TestEditBlockedInReadOnlyProfile(t *testing.T) {
	server := fakeS3(t)
	before, _ := server.config("policy", "bucket-a")

	withEditor(t, writing(`{"Version":"2012-10-17","Statement":[]}`))

	model := openConfig(t, server, configPolicy)
	inner := model.(Model)
	inner.profileCfg.ReadOnly = true

	model = press(t, tea.Model(inner), 'e')

	if after, _ := server.config("policy", "bucket-a"); after != before {
		t.Errorf("a read-only profile wrote the policy:\n%s", after)
	}
	if status := model.(Model).status; !strings.Contains(status, "read-only") {
		t.Errorf("status = %q, want the read-only note", status)
	}
}

func TestDeleteBucketPolicy(t *testing.T) {
	server := fakeS3(t)
	model := openConfig(t, server, configPolicy)

	model = press(t, model, 'd')
	if model.(Model).pending == nil {
		t.Fatal("removing the policy was not confirmed first")
	}
	if question := model.(Model).pending.question; !strings.Contains(question, "bucket-a") {
		t.Errorf("question = %q, want it to name the bucket", question)
	}

	model = press(t, model, 'y')

	if _, ok := server.config("policy", "bucket-a"); ok {
		t.Error("the bucket policy is still there")
	}

	got := model.(Model)
	if got.err != nil {
		t.Errorf("unexpected error: %v", got.err)
	}

	// the reload shows the bucket without a policy
	view := got.View() + got.detailsContent
	if !strings.Contains(view, "no bucket policy") {
		t.Errorf("the view still shows a policy:\n%s", view)
	}
}

func TestDeleteLifecycleCanBeCanceled(t *testing.T) {
	server := fakeS3(t)
	model := openConfig(t, server, configLifecycle)

	model = press(t, model, 'd')
	model = press(t, model, 'n')

	if _, ok := server.config("lifecycle", "bucket-a"); !ok {
		t.Error("the lifecycle configuration was deleted although the question was answered with no")
	}
	if status := model.(Model).status; status != "canceled" {
		t.Errorf("status = %q, want the canceled note", status)
	}
}

func TestDeleteBlockedInReadOnlyProfileConfig(t *testing.T) {
	server := fakeS3(t)

	model := openConfig(t, server, configLifecycle)
	inner := model.(Model)
	inner.profileCfg.ReadOnly = true

	model = press(t, tea.Model(inner), 'd')

	if model.(Model).pending != nil {
		t.Error("a read-only profile asked to delete the configuration")
	}
	if _, ok := server.config("lifecycle", "bucket-a"); !ok {
		t.Error("a read-only profile deleted the configuration")
	}
}

func TestEscLeavesAConfigViewAfterAnError(t *testing.T) {
	server := fakeS3(t)

	for _, kind := range []configKind{configPolicy, configLifecycle} {
		model := openConfig(t, server, kind)
		opened := model.(Model).view

		model = step(t, model, errMsg{what: "saving", err: errors.New("boom")})
		if !strings.Contains(model.View(), "boom") {
			t.Fatalf("no error box:\n%s", model.View())
		}

		model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc}) // closes the box
		if got := model.(Model).view; got != opened {
			t.Fatalf("closing the box left the view: %v, want %v", got, opened)
		}

		// and esc still walks up instead of bouncing back
		model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
		if got := model.(Model).view; got != viewBucketMenu {
			t.Errorf("esc went to %v, want the bucket overview", got)
		}
	}
}

func TestTheOtherBucketConfigViews(t *testing.T) {
	server := fakeS3(t)

	for _, test := range []struct {
		kind configKind
		want []string
	}{
		{configACL, []string{"[acl]", "bucket ACL", `"DisplayName": "simon"`, `"Permission": "FULL_CONTROL"`}},
		{configPublicAccess, []string{
			"[public-access]", "public access block",
			`"BlockPublicAcls": true`, `"BlockPublicPolicy": false`,
		}},
		{configCORS, []string{
			"[cors]", "CORS configuration", `"ID": "web"`, `"AllowedOrigins"`,
			"https://example.com", `"MaxAgeSeconds": 3000`,
		}},
		{configVersioning, []string{"[versioning]", "versioning configuration", `"Status": "Enabled"`}},
		{configObjectLock, []string{
			"[object-lock]", "object lock configuration", `"Mode": "GOVERNANCE"`, `"Days": 30`,
		}},
	} {
		model := openConfig(t, server, test.kind)

		got := model.(Model)
		if got.view != viewConfig || got.configKind != test.kind {
			t.Fatalf("view = %v/%v, want %v", got.view, got.configKind, test.kind)
		}

		view := model.View() + got.detailsContent
		for _, want := range test.want {
			if !strings.Contains(view, want) {
				t.Errorf("the %s view misses %q:\n%s", test.kind.spec().entry, want, view)
			}
		}
		if got.err != nil {
			t.Errorf("%s: unexpected error: %v", test.kind.spec().entry, got.err)
		}
	}
}

func TestEditCORS(t *testing.T) {
	server := fakeS3(t)

	withEditor(t, writing(`{"CORSRules":[{"AllowedMethods":["PUT"],"AllowedOrigins":["https://s3tui.test"]}]}`))

	model := press(t, openConfig(t, server, configCORS), 'e')

	stored, ok := server.config("cors", "bucket-a")
	if !ok {
		t.Fatal("the CORS configuration was removed instead of written")
	}
	for _, want := range []string{"<AllowedMethod>PUT</AllowedMethod>", "<AllowedOrigin>https://s3tui.test</AllowedOrigin>"} {
		if !strings.Contains(stored, want) {
			t.Errorf("the stored configuration misses %q:\n%s", want, stored)
		}
	}
	if strings.Contains(stored, "example.com") {
		t.Errorf("the old rule was kept:\n%s", stored)
	}

	got := model.(Model)
	if got.err != nil {
		t.Errorf("unexpected error: %v", got.err)
	}
	if !strings.Contains(got.View()+got.detailsContent, "s3tui.test") {
		t.Errorf("the view was not reloaded:\n%s", got.detailsContent)
	}
}

func TestEditVersioning(t *testing.T) {
	server := fakeS3(t)

	withEditor(t, writing(`{"Status":"Suspended"}`))

	model := press(t, openConfig(t, server, configVersioning), 'e')

	stored, _ := server.config("versioning", "bucket-a")
	if !strings.Contains(stored, "<Status>Suspended</Status>") {
		t.Errorf("versioning was not suspended:\n%s", stored)
	}

	got := model.(Model)
	if got.err != nil {
		t.Errorf("unexpected error: %v", got.err)
	}

	// the breadcrumb of the object views reads the same state, it is refetched
	if status := got.versioning["bucket-a"]; status != versioningSuspend {
		t.Errorf("the remembered versioning state is %q, want it refreshed", status)
	}
	if got.inFlight != 0 {
		t.Errorf("inFlight = %d, the two refetches did not both report back", got.inFlight)
	}
}

func TestEditRejectsAnUnknownField(t *testing.T) {
	server := fakeS3(t)
	before, _ := server.config("cors", "bucket-a")

	withEditor(t, writing(`{"CORSRules":[{"AllowedMethod":["GET"]}]}`)) // AllowedMethods

	model := press(t, openConfig(t, server, configCORS), 'e')

	if after, _ := server.config("cors", "bucket-a"); after != before {
		t.Errorf("a document with a typo was written:\n%s", after)
	}

	got := model.(Model)
	if got.err == nil || !strings.Contains(got.err.Error(), "not readable") {
		t.Errorf("err = %v, want a complaint about the field", got.err)
	}
}

func TestDeleteCORS(t *testing.T) {
	server := fakeS3(t)

	model := press(t, openConfig(t, server, configCORS), 'd')
	if model.(Model).pending == nil {
		t.Fatal("deleting the CORS configuration was not confirmed first")
	}

	model = press(t, model, 'y')

	if _, ok := server.config("cors", "bucket-a"); ok {
		t.Error("the CORS configuration is still there")
	}
	if view := model.View() + model.(Model).detailsContent; !strings.Contains(view, "no CORS configuration") {
		t.Errorf("the view still shows rules:\n%s", view)
	}
}

func TestWhatCannotBeDeletedSaysSo(t *testing.T) {
	server := fakeS3(t)

	for _, kind := range []configKind{configACL, configVersioning, configObjectLock} {
		model := press(t, openConfig(t, server, kind), 'd')

		got := model.(Model)
		if got.pending != nil {
			t.Fatalf("%s asked to delete something the API cannot delete", kind.spec().entry)
		}
		if !strings.Contains(got.status, "cannot be deleted") {
			t.Errorf("%s: status = %q", kind.spec().entry, got.status)
		}
	}
}

func TestEmptyDocumentOfWhatCannotBeDeleted(t *testing.T) {
	server := fakeS3(t)

	withEditor(t, writing("\n"))

	model := press(t, openConfig(t, server, configACL), 'e')

	got := model.(Model)
	if got.err == nil || !strings.Contains(got.err.Error(), "nothing to save") {
		t.Errorf("err = %v, want the note that an empty ACL saves nothing", got.err)
	}
}

func TestConfigTemplateOpensInsteadOfAnEmptyFile(t *testing.T) {
	server := fakeS3(t)

	model := newTestModel(t, server.URL)
	inner := model.(Model)
	inner.buckets.Select(1) // bucket-b has no CORS configuration
	model = openConfigSection(t, enter(t, tea.Model(inner)), configCORS)

	if got := model.(Model).configDocument(); got != configCORS.spec().template {
		t.Errorf("the editor would open on %q, want the template", got)
	}
}

// openObjectConfig walks to file.txt of bucket-a and opens one of its
// documents.
func openObjectConfig(t *testing.T, server *fakeServer, kind configKind) tea.Model {
	t.Helper()

	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt

	model = enter(t, tea.Model(inner)) // its versions
	model = enter(t, model)            // the overview of the newest one

	return openConfigSection(t, model, kind)
}

func TestObjectOverviewIsTheWayIn(t *testing.T) {
	server := fakeS3(t)
	model := openBucket(t, server)

	inner := model.(Model)
	inner.objects.Select(1) // file.txt
	model = enter(t, tea.Model(inner))
	model = enter(t, model)

	got := model.(Model)
	if got.view != viewObjectMenu {
		t.Fatalf("view = %v, want the object overview", got.view)
	}
	if got.objectKey != "file.txt" || got.objectVersion != "v2" {
		t.Errorf("the overview is about %q/%q, want the newest version of file.txt", got.objectKey, got.objectVersion)
	}
	if want := len(configEntries(true)); len(got.objectMenu.Items()) != want {
		t.Fatalf("got %d entries, want %d", len(got.objectMenu.Items()), want)
	}

	view := model.View()
	for _, want := range []string{"file.txt", "metadata", "tags", "acl", "legal hold", "retention", "enter open", "esc back"} {
		if !strings.Contains(view, want) {
			t.Errorf("the object overview misses %q:\n%s", want, view)
		}
	}

	// the bucket configurations stay where they are
	if strings.Contains(view, "lifecycle") || strings.Contains(view, "cors") {
		t.Errorf("the object overview lists bucket configurations:\n%s", view)
	}
}

func TestObjectACLView(t *testing.T) {
	server := fakeS3(t)
	model := openObjectConfig(t, server, configObjectACL)

	got := model.(Model)
	if got.view != viewConfig || got.configKind != configObjectACL {
		t.Fatalf("view = %v/%v, want the object ACL", got.view, got.configKind)
	}
	if got.target != (configTarget{bucket: "bucket-a", key: "file.txt", version: "v2"}) {
		t.Errorf("target = %+v, want the version the overview was opened for", got.target)
	}

	view := model.View() + got.detailsContent
	for _, want := range []string{"file.txt", "[acl]", "object ACL", `"Permission": "FULL_CONTROL"`} {
		if !strings.Contains(view, want) {
			t.Errorf("the ACL view misses %q:\n%s", want, view)
		}
	}

	// esc walks back to the overview of the object, not to the bucket
	model = step(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if got := model.(Model).view; got != viewObjectMenu {
		t.Errorf("view = %v, want the object overview", got)
	}
}

func TestLegalHoldView(t *testing.T) {
	server := fakeS3(t)
	model := openObjectConfig(t, server, configObjectLegalHold)

	got := model.(Model)
	view := model.View() + got.detailsContent

	for _, want := range []string{"[legal-hold]", "legal hold", `"Status": "ON"`} {
		if !strings.Contains(view, want) {
			t.Errorf("the legal hold view misses %q:\n%s", want, view)
		}
	}
	if got.err != nil {
		t.Errorf("unexpected error: %v", got.err)
	}
}

func TestObjectRetentionView(t *testing.T) {
	server := fakeS3(t)
	model := openObjectConfig(t, server, configObjectRetention)

	got := model.(Model)
	view := model.View() + got.detailsContent

	for _, want := range []string{
		"[retention]", "object retention", `"Mode": "GOVERNANCE"`,
		`"RetainUntilDate": "2030-01-02T03:04:05Z"`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the retention view misses %q:\n%s", want, view)
		}
	}
	if got.err != nil {
		t.Errorf("unexpected error: %v", got.err)
	}
}

func TestEditLegalHold(t *testing.T) {
	server := fakeS3(t)

	withEditor(t, writing(`{"Status":"OFF"}`))

	model := press(t, openObjectConfig(t, server, configObjectLegalHold), 'e')

	stored, _ := server.config("legal-hold", "bucket-a/file.txt")
	if !strings.Contains(stored, "<Status>OFF</Status>") {
		t.Errorf("the legal hold was not lifted:\n%s", stored)
	}

	got := model.(Model)
	if got.err != nil {
		t.Errorf("unexpected error: %v", got.err)
	}
	if !strings.Contains(got.View()+got.detailsContent, `"Status": "OFF"`) {
		t.Errorf("the view was not reloaded:\n%s", got.detailsContent)
	}
}

func TestEditObjectACL(t *testing.T) {
	server := fakeS3(t)

	withEditor(t, writing(`{"Owner":{"ID":"owner-id"},"Grants":[{"Grantee":{"Type":"Group","URI":"http://acs.amazonaws.com/groups/global/AllUsers"},"Permission":"READ"}]}`))

	model := press(t, openObjectConfig(t, server, configObjectACL), 'e')

	stored, ok := server.config("acl", "bucket-a/file.txt")
	if !ok {
		t.Fatal("the object ACL was not written")
	}
	if !strings.Contains(stored, "<Permission>READ</Permission>") {
		t.Errorf("the stored ACL misses the grant:\n%s", stored)
	}

	// the bucket keeps its own ACL, the object is a target of its own
	if _, ok := server.config("acl", "bucket-a"); ok {
		t.Error("editing the object ACL wrote the bucket ACL")
	}

	if err := model.(Model).err; err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestObjectConfigFooters(t *testing.T) {
	server := fakeS3(t)

	// neither an ACL nor a legal hold can be deleted, only replaced
	for _, kind := range []configKind{configObjectACL, configObjectLegalHold} {
		footer := openObjectConfig(t, server, kind).View()

		if !strings.Contains(footer, "e edit") {
			t.Errorf("the footer of %v misses the edit key:\n%s", kind, footer)
		}
		if strings.Contains(footer, "d delete") {
			t.Errorf("the footer of %v offers a delete which does not exist:\n%s", kind, footer)
		}
	}

	footer := openObjectConfig(t, server, configObjectRetention).View()
	if strings.Contains(footer, "e edit") || strings.Contains(footer, "d delete") {
		t.Errorf("the read-only retention footer offers a mutation:\n%s", footer)
	}
}

func TestObjectWithoutALegalHold(t *testing.T) {
	server := fakeS3(t)

	model := enter(t, openBucket(t, server)) // into logs/

	inner := model.(Model)
	inner.objects.Select(0) // logs/nested.log, which has no hold

	model = enter(t, tea.Model(inner)) // its versions
	model = openConfigSection(t, enter(t, model), configObjectLegalHold)

	got := model.(Model)
	view := model.View() + got.detailsContent

	if !strings.Contains(view, "no legal hold") {
		t.Errorf("the view misses the note:\n%s", view)
	}
	if got.err != nil {
		t.Errorf("a missing legal hold must not be an error: %v", got.err)
	}
}

func TestObjectWithoutRetention(t *testing.T) {
	server := fakeS3(t)

	model := enter(t, openBucket(t, server)) // into logs/

	inner := model.(Model)
	inner.objects.Select(0) // logs/nested.log, which has no retention

	model = enter(t, tea.Model(inner)) // its versions
	model = openConfigSection(t, enter(t, model), configObjectRetention)

	got := model.(Model)
	view := model.View() + got.detailsContent

	if !strings.Contains(view, "no retention") {
		t.Errorf("the view misses the note:\n%s", view)
	}
	if got.err != nil {
		t.Errorf("missing retention must not be an error: %v", got.err)
	}
}

func TestEditObjectTags(t *testing.T) {
	server := fakeS3(t)

	withEditor(t, writing(`{"team":"platform","env":"staging"}`))

	model := press(t, openObjectConfig(t, server, configObjectTags), 'e')

	stored, ok := server.config("tagging", "bucket-a/file.txt")
	if !ok {
		t.Fatal("the tags were removed instead of written")
	}
	for _, want := range []string{"<Key>team</Key>", "<Value>staging</Value>"} {
		if !strings.Contains(stored, want) {
			t.Errorf("the stored tag set misses %q:\n%s", want, stored)
		}
	}
	if strings.Contains(stored, "<Value>prod</Value>") {
		t.Errorf("the old value was kept:\n%s", stored)
	}

	got := model.(Model)
	if got.err != nil {
		t.Errorf("unexpected error: %v", got.err)
	}
	if !strings.Contains(got.View()+got.detailsContent, `"team": "platform"`) {
		t.Errorf("the view was not reloaded:\n%s", got.detailsContent)
	}
}

func TestDeleteObjectTags(t *testing.T) {
	server := fakeS3(t)

	model := press(t, openObjectConfig(t, server, configObjectTags), 'd')
	if model.(Model).pending == nil {
		t.Fatal("removing the tags was not confirmed first")
	}
	if question := model.(Model).pending.question; !strings.Contains(question, "file.txt") {
		t.Errorf("question = %q, want it to name the object", question)
	}

	model = press(t, model, 'y')

	if _, ok := server.config("tagging", "bucket-a/file.txt"); ok {
		t.Error("the tags are still there")
	}
	if view := model.View() + model.(Model).detailsContent; !strings.Contains(view, "no tags") {
		t.Errorf("the view still shows tags:\n%s", view)
	}
}
