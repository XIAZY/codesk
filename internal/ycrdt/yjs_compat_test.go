package ycrdt

import (
	"encoding/base64"
	"os/exec"
	"testing"
)

func TestYCRDTStickyIndexIsYjsRelativePositionCompatible(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not available")
	}
	doc := New(WithClientID(11))
	defer doc.Close()
	text := doc.GetText("content")
	if _, err := doc.Update(func(txn *Transaction) error {
		text.Insert(txn, 0, "hello world", nil)
		return nil
	}, "seed"); err != nil {
		t.Fatalf("seed doc: %v", err)
	}
	update := doc.EncodeStateAsUpdate()
	anchor, err := text.EncodeRelativeAnchor(6, 0)
	if err != nil {
		t.Fatalf("encode anchor: %v", err)
	}

	script := `
const Y = require('yjs');
const update = Buffer.from(process.argv[1], 'base64');
const anchor = Buffer.from(process.argv[2], 'base64');
const doc = new Y.Doc();
Y.applyUpdate(doc, update);
const text = doc.getText('content');
const before = Y.createAbsolutePositionFromRelativePosition(Y.decodeRelativePosition(anchor), doc);
if (!before || before.index !== 6) {
  throw new Error('anchor decoded to ' + JSON.stringify(before));
}
text.insert(0, 'x');
const after = Y.createAbsolutePositionFromRelativePosition(Y.decodeRelativePosition(anchor), doc);
if (!after || after.index !== 7) {
  throw new Error('anchor did not move after insert: ' + JSON.stringify(after));
}
`
	cmd := exec.Command("node", "-e", script, base64.StdEncoding.EncodeToString(update), base64.StdEncoding.EncodeToString(anchor))
	cmd.Dir = "../../frontend"
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node yjs compatibility check failed: %v\n%s", err, output)
	}
}
