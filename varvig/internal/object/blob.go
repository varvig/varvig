package object

// NewBlob builds a blob object holding raw content.
func NewBlob(content []byte) *Object {
	return newObject(TypeBlob, []field{{tag: tagBlobContent, val: append([]byte(nil), content...)}})
}

// BlobContent returns the content of a blob object.
func (o *Object) BlobContent() ([]byte, bool) {
	if o.typ != TypeBlob {
		return nil, false
	}
	return o.Field(tagBlobContent)
}
