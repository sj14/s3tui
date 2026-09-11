package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// configKind is one of the documents a bucket is configured with. They all
// behave the same in the UI: read as JSON, edited in $EDITOR, written back.
type configKind int

const (
	configPolicy configKind = iota
	configLifecycle
	configACL
	configPublicAccess
	configCORS
	configVersioning
	configObjectLock
	configObjectMetadata
	configObjectTags
	configObjectACL
	configObjectLegalHold
	configObjectRetention
)

// configTarget is what a configuration belongs to: a whole bucket, or one
// object or version in it.
type configTarget struct {
	bucket  string
	key     string // empty for a bucket configuration
	version string
}

func (t configTarget) name() string {
	if t.key == "" {
		return t.bucket
	}

	return t.key
}

// versionID is the version to address, nil for the newest one.
func (t configTarget) versionID() *string {
	if t.version == "" {
		return nil
	}

	return aws.String(t.version)
}

// configSpec is everything the UI needs to know about one configuration:
// how it is named, and how it is read, written and taken away again. A nil
// remove means the API has no call for it – versioning can only be suspended,
// an ACL and an object lock configuration only replaced.
type configSpec struct {
	name     string // how the document is named in titles and messages
	short    string // breadcrumb label and name of the file the editor opens
	entry    string // how the bucket overview lists it
	about    string // the line below that entry
	missing  string // note for a bucket or object which has none
	template string // what the editor starts from when there is none
	object   bool   // belongs to a single object, not to the whole bucket
	get      func(context.Context, *s3.Client, configTarget) (string, error)
	put      func(context.Context, *s3.Client, configTarget, string) error
	remove   func(context.Context, *s3.Client, configTarget) error
}

func (k configKind) spec() configSpec { return configSpecs[k] }

// configSpecs is indexed by configKind and is also the order of the bucket
// overview.
var configSpecs = []configSpec{
	configPolicy: {
		name:     "bucket policy",
		short:    "policy",
		entry:    "policy",
		about:    "the bucket policy, editable",
		missing:  "no bucket policy",
		template: "{\n  \"Version\": \"2012-10-17\",\n  \"Statement\": []\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(target.bucket)})
			if err != nil {
				return "", err
			}

			return indentPolicy(aws.ToString(resp.Policy)), nil
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			// The statements are S3's judgement, only the syntax is ours.
			if !json.Valid([]byte(document)) {
				return invalid("the policy is no valid JSON")
			}

			_, err := client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
				Bucket: aws.String(target.bucket),
				Policy: aws.String(document),
			})

			return err
		},
		remove: func(ctx context.Context, client *s3.Client, target configTarget) error {
			_, err := client.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(target.bucket)})

			return err
		},
	},

	configLifecycle: {
		name:     "lifecycle configuration",
		short:    "lifecycle",
		entry:    "lifecycle",
		about:    "the lifecycle rules of the bucket, editable",
		missing:  "no lifecycle configuration",
		template: "{\n  \"Rules\": []\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
				Bucket: aws.String(target.bucket),
			})
			if err != nil {
				return "", err
			}

			if len(resp.Rules) == 0 {
				return "", nil
			}

			return documentOf(types.BucketLifecycleConfiguration{Rules: resp.Rules})
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			parsed, err := parseDocument[types.BucketLifecycleConfiguration]("lifecycle configuration", document)
			if err != nil {
				return err
			}

			if len(parsed.Rules) == 0 {
				return invalid(`no rules below "Rules", d deletes the configuration`)
			}

			_, err = client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
				Bucket:                 aws.String(target.bucket),
				LifecycleConfiguration: &parsed,
			})

			return err
		},
		remove: func(ctx context.Context, client *s3.Client, target configTarget) error {
			_, err := client.DeleteBucketLifecycle(ctx, &s3.DeleteBucketLifecycleInput{Bucket: aws.String(target.bucket)})

			return err
		},
	},

	configACL: {
		name:     "bucket ACL",
		short:    "acl",
		entry:    "acl",
		about:    "owner and grants of the bucket, editable",
		missing:  "no bucket ACL",
		template: "{\n  \"Owner\": {\n    \"ID\": \"\"\n  },\n  \"Grants\": []\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetBucketAcl(ctx, &s3.GetBucketAclInput{Bucket: aws.String(target.bucket)})
			if err != nil {
				return "", err
			}

			return documentOf(types.AccessControlPolicy{Grants: resp.Grants, Owner: resp.Owner})
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			parsed, err := parseDocument[types.AccessControlPolicy]("bucket ACL", document)
			if err != nil {
				return err
			}

			if parsed.Owner == nil {
				return invalid(`the ACL needs an "Owner", S3 refuses it without one`)
			}

			_, err = client.PutBucketAcl(ctx, &s3.PutBucketAclInput{
				Bucket:              aws.String(target.bucket),
				AccessControlPolicy: &parsed,
			})

			return err
		},
	},

	configPublicAccess: {
		name:    "public access block",
		short:   "public-access",
		entry:   "public access block",
		about:   "which public ACLs and policies the bucket refuses, editable",
		missing: "no public access block",
		template: "{\n  \"BlockPublicAcls\": true,\n  \"IgnorePublicAcls\": true,\n" +
			"  \"BlockPublicPolicy\": true,\n  \"RestrictPublicBuckets\": true\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetPublicAccessBlock(ctx, &s3.GetPublicAccessBlockInput{Bucket: aws.String(target.bucket)})
			if err != nil {
				return "", err
			}

			if resp.PublicAccessBlockConfiguration == nil {
				return "", nil
			}

			return documentOf(resp.PublicAccessBlockConfiguration)
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			parsed, err := parseDocument[types.PublicAccessBlockConfiguration]("public access block", document)
			if err != nil {
				return err
			}

			_, err = client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
				Bucket:                         aws.String(target.bucket),
				PublicAccessBlockConfiguration: &parsed,
			})

			return err
		},
		remove: func(ctx context.Context, client *s3.Client, target configTarget) error {
			_, err := client.DeletePublicAccessBlock(ctx, &s3.DeletePublicAccessBlockInput{Bucket: aws.String(target.bucket)})

			return err
		},
	},

	configCORS: {
		name:     "CORS configuration",
		short:    "cors",
		entry:    "cors",
		about:    "which origins may call the bucket from a browser, editable",
		missing:  "no CORS configuration",
		template: "{\n  \"CORSRules\": []\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(target.bucket)})
			if err != nil {
				return "", err
			}

			if len(resp.CORSRules) == 0 {
				return "", nil
			}

			return documentOf(types.CORSConfiguration{CORSRules: resp.CORSRules})
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			parsed, err := parseDocument[types.CORSConfiguration]("CORS configuration", document)
			if err != nil {
				return err
			}

			if len(parsed.CORSRules) == 0 {
				return invalid(`no rules below "CORSRules", d deletes the configuration`)
			}

			_, err = client.PutBucketCors(ctx, &s3.PutBucketCorsInput{
				Bucket:            aws.String(target.bucket),
				CORSConfiguration: &parsed,
			})

			return err
		},
		remove: func(ctx context.Context, client *s3.Client, target configTarget) error {
			_, err := client.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: aws.String(target.bucket)})

			return err
		},
	},

	configVersioning: {
		name:     "versioning configuration",
		short:    "versioning",
		entry:    "versioning",
		about:    "whether the bucket keeps older versions, editable",
		missing:  "versioning was never enabled",
		template: "{\n  \"Status\": \"Enabled\"\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(target.bucket)})
			if err != nil {
				return "", err
			}

			if resp.Status == "" && resp.MFADelete == "" {
				return "", nil
			}

			return documentOf(versioningDocument{
				Status:    resp.Status,
				MFADelete: types.MFADelete(resp.MFADelete),
			})
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			parsed, err := parseDocument[versioningDocument]("versioning configuration", document)
			if err != nil {
				return err
			}

			if parsed.Status == "" {
				return invalid(`"Status" has to be "Enabled" or "Suspended", versioning cannot be turned off`)
			}

			_, err = client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
				Bucket: aws.String(target.bucket),
				VersioningConfiguration: &types.VersioningConfiguration{
					Status:    parsed.Status,
					MFADelete: parsed.MFADelete,
				},
			})

			return err
		},
	},

	configObjectLock: {
		name:     "object lock configuration",
		short:    "object-lock",
		entry:    "object lock",
		about:    "the default retention of the bucket, editable",
		missing:  "no object lock configuration",
		template: "{\n  \"ObjectLockEnabled\": \"Enabled\"\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{
				Bucket: aws.String(target.bucket),
			})
			if err != nil {
				return "", err
			}

			if resp.ObjectLockConfiguration == nil {
				return "", nil
			}

			return documentOf(resp.ObjectLockConfiguration)
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			parsed, err := parseDocument[types.ObjectLockConfiguration]("object lock configuration", document)
			if err != nil {
				return err
			}

			_, err = client.PutObjectLockConfiguration(ctx, &s3.PutObjectLockConfigurationInput{
				Bucket:                  aws.String(target.bucket),
				ObjectLockConfiguration: &parsed,
			})

			return err
		},
	},

	configObjectMetadata: {
		name:    "metadata",
		short:   "metadata",
		entry:   "metadata",
		about:   "what HeadObject knows about the object",
		missing: "no metadata",
		object:  true,

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket:    aws.String(target.bucket),
				Key:       aws.String(target.key),
				VersionId: target.versionID(),
			})
			if err != nil {
				return "", err
			}

			return headJSON(head)
		},
	},

	configObjectTags: {
		name:     "tag set",
		short:    "tags",
		entry:    "tags",
		about:    "the tags of the object, editable",
		missing:  "no tags",
		object:   true,
		template: "{\n  \"env\": \"prod\"\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
				Bucket:    aws.String(target.bucket),
				Key:       aws.String(target.key),
				VersionId: target.versionID(),
			})
			if err != nil {
				return "", err
			}

			if len(resp.TagSet) == 0 {
				return "", nil
			}

			return documentOf(tagDocument(resp.TagSet))
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			parsed, err := parseDocument[map[string]string]("tag set", document)
			if err != nil {
				return err
			}

			if len(parsed) == 0 {
				return invalid("no tags in the document, d removes the whole tag set")
			}

			_, err = client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
				Bucket:    aws.String(target.bucket),
				Key:       aws.String(target.key),
				VersionId: target.versionID(),
				Tagging:   &types.Tagging{TagSet: tagSet(parsed)},
			})

			return err
		},
		remove: func(ctx context.Context, client *s3.Client, target configTarget) error {
			_, err := client.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
				Bucket:    aws.String(target.bucket),
				Key:       aws.String(target.key),
				VersionId: target.versionID(),
			})

			return err
		},
	},

	configObjectACL: {
		name:     "object ACL",
		short:    "acl",
		entry:    "acl",
		about:    "owner and grants of the object, editable",
		missing:  "no object ACL",
		object:   true,
		template: "{\n  \"Owner\": {\n    \"ID\": \"\"\n  },\n  \"Grants\": []\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetObjectAcl(ctx, &s3.GetObjectAclInput{
				Bucket:    aws.String(target.bucket),
				Key:       aws.String(target.key),
				VersionId: target.versionID(),
			})
			if err != nil {
				return "", err
			}

			return documentOf(types.AccessControlPolicy{Grants: resp.Grants, Owner: resp.Owner})
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			parsed, err := parseDocument[types.AccessControlPolicy]("object ACL", document)
			if err != nil {
				return err
			}

			if parsed.Owner == nil {
				return invalid(`the ACL needs an "Owner", S3 refuses it without one`)
			}

			_, err = client.PutObjectAcl(ctx, &s3.PutObjectAclInput{
				Bucket:              aws.String(target.bucket),
				Key:                 aws.String(target.key),
				VersionId:           target.versionID(),
				AccessControlPolicy: &parsed,
			})

			return err
		},
	},

	configObjectLegalHold: {
		name:     "legal hold",
		short:    "legal-hold",
		entry:    "legal hold",
		about:    "whether the object is locked against deletion, editable",
		missing:  "no legal hold",
		object:   true,
		template: "{\n  \"Status\": \"ON\"\n}\n",

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetObjectLegalHold(ctx, &s3.GetObjectLegalHoldInput{
				Bucket:    aws.String(target.bucket),
				Key:       aws.String(target.key),
				VersionId: target.versionID(),
			})
			if err != nil {
				// A bucket without object lock answers with a plain
				// InvalidRequest, which says nothing to read here either.
				if apiErr, ok := errors.AsType[smithy.APIError](err); ok && apiErr.ErrorCode() == "InvalidRequest" {
					return "", nil
				}

				return "", err
			}

			if resp.LegalHold == nil {
				return "", nil
			}

			return documentOf(resp.LegalHold)
		},
		put: func(ctx context.Context, client *s3.Client, target configTarget, document string) error {
			parsed, err := parseDocument[types.ObjectLockLegalHold]("legal hold", document)
			if err != nil {
				return err
			}

			if parsed.Status == "" {
				return invalid(`"Status" has to be "ON" or "OFF"`)
			}

			_, err = client.PutObjectLegalHold(ctx, &s3.PutObjectLegalHoldInput{
				Bucket:    aws.String(target.bucket),
				Key:       aws.String(target.key),
				VersionId: target.versionID(),
				LegalHold: &parsed,
			})

			return err
		},
	},

	configObjectRetention: {
		name:    "object retention",
		short:   "retention",
		entry:   "retention",
		about:   "how long the object is locked against deletion",
		missing: "no retention",
		object:  true,

		get: func(ctx context.Context, client *s3.Client, target configTarget) (string, error) {
			resp, err := client.GetObjectRetention(ctx, &s3.GetObjectRetentionInput{
				Bucket:    aws.String(target.bucket),
				Key:       aws.String(target.key),
				VersionId: target.versionID(),
			})
			if err != nil {
				// A bucket without object lock answers with a plain
				// InvalidRequest, which means there is no retention to show.
				if apiErr, ok := errors.AsType[smithy.APIError](err); ok && apiErr.ErrorCode() == "InvalidRequest" {
					return "", nil
				}

				return "", err
			}

			if resp.Retention == nil ||
				(resp.Retention.Mode == "" && resp.Retention.RetainUntilDate == nil) {
				return "", nil
			}

			return documentOf(resp.Retention)
		},
	},
}

// tagDocument turns a tag set into the object the view shows. Tags are the one
// document which is not the SDK struct: a flat object reads and edits better
// than a list of Key/Value pairs, and their order means nothing to S3.
func tagDocument(tags []types.Tag) map[string]string {
	document := make(map[string]string, len(tags))
	for _, tag := range tags {
		document[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
	}

	return document
}

func tagSet(document map[string]string) []types.Tag {
	tags := make([]types.Tag, 0, len(document))
	for key, value := range document {
		tags = append(tags, types.Tag{Key: aws.String(key), Value: aws.String(value)})
	}

	sort.Slice(tags, func(i, j int) bool { return *tags[i].Key < *tags[j].Key })

	return tags
}

// versioningDocument is the versioning configuration as the view shows it.
// The SDK reads the status into another type than it writes it, and a bucket
// which never saw an MFA device should not be shown an empty field for it.
type versioningDocument struct {
	Status    types.BucketVersioningStatus `json:",omitempty"`
	MFADelete types.MFADelete              `json:",omitempty"`
}

// indentPolicy pretty prints the policy document, unchanged if it is no JSON.
func indentPolicy(policy string) string {
	var buf bytes.Buffer

	if err := json.Indent(&buf, []byte(policy), "", "  "); err != nil {
		return strings.TrimSpace(policy)
	}

	return buf.String()
}

// headJSON renders the metadata of an object. ResultMetadata is dropped, it
// carries SDK internals and no information about the object.
func headJSON(head *s3.HeadObjectOutput) (string, error) {
	raw, err := json.Marshal(head)
	if err != nil {
		return "", err
	}

	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", err
	}

	delete(document, "ResultMetadata")

	pretty, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", err
	}

	return string(pretty), nil
}

// documentOf renders an SDK configuration as the JSON the view shows and the
// editor works on. S3 speaks XML, so this is the SDK's view of the document.
func documentOf(value any) (string, error) {
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}

	return string(pretty), nil
}

// parseDocument reads the document back into the shape the view renders.
// Unknown fields are rejected: a typo would silently drop the setting it
// belongs to.
func parseDocument[T any](name, document string) (T, error) {
	var parsed T

	decoder := json.NewDecoder(strings.NewReader(document))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&parsed); err != nil {
		return parsed, invalid("the %s is not readable: %v", name, err)
	}

	return parsed, nil
}

// documentError is a problem with what the user typed. Nothing was sent, so it
// is shown as it is instead of below a "saving the …".
type documentError struct{ text string }

func (e documentError) Error() string { return e.text }

func invalid(format string, args ...any) error {
	return documentError{text: fmt.Sprintf(format, args...)}
}
