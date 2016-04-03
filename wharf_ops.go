package main

import (
	"io/ioutil"
	"os"
	"path"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/itchio/butler/comm"
	"github.com/itchio/wharf/counter"
	"github.com/itchio/wharf/pwr"
	"github.com/itchio/wharf/sync"
	"github.com/itchio/wharf/tlc"
	"github.com/itchio/wharf/wire"
)

func diff(target string, source string, patch string, brotliQuality int) {
	must(doDiff(target, source, patch, brotliQuality))
}

func doDiff(target string, source string, patch string, brotliQuality int) error {
	startTime := time.Now()

	var targetSignature []sync.BlockHash
	var targetContainer *tlc.Container

	if target == "/dev/null" {
		targetContainer = &tlc.Container{}
	} else {
		targetInfo, err := os.Lstat(target)
		if err != nil {
			return err
		}

		if targetInfo.IsDir() {
			comm.Opf("Hashing %s", target)
			targetContainer, err = tlc.Walk(target, filterPaths)
			if err != nil {
				return err
			}

			comm.StartProgress()
			targetSignature, err = pwr.ComputeSignature(targetContainer, target, comm.NewStateConsumer())
			comm.EndProgress()
			if err != nil {
				return err
			}

			{
				prettySize := humanize.Bytes(uint64(targetContainer.Size))
				perSecond := humanize.Bytes(uint64(float64(targetContainer.Size) / time.Since(startTime).Seconds()))
				comm.Statf("%s (%s) @ %s/s\n", prettySize, targetContainer.Stats(), perSecond)
			}
		} else {
			comm.Opf("Reading signature from %s", target)
			signatureReader, err := os.Open(target)
			if err != nil {
				return err
			}
			targetContainer, targetSignature, err = pwr.ReadSignature(signatureReader)
			if err != nil {
				return err
			}
			err = signatureReader.Close()
			if err != nil {
				return err
			}
		}

	}

	startTime = time.Now()

	var sourceContainer *tlc.Container
	if source == "/dev/null" {
		sourceContainer = &tlc.Container{}
	} else {
		var err error
		sourceContainer, err = tlc.Walk(source, filterPaths)
		if err != nil {
			return err
		}
	}

	patchWriter, err := os.Create(patch)
	if err != nil {
		return err
	}
	defer patchWriter.Close()

	signaturePath := patch + ".sig"
	signatureWriter, err := os.Create(signaturePath)
	if err != nil {
		return err
	}
	defer signatureWriter.Close()

	patchCounter := counter.NewWriter(patchWriter)
	signatureCounter := counter.NewWriter(signatureWriter)

	dctx := &pwr.DiffContext{
		SourceContainer: sourceContainer,
		SourcePath:      source,

		TargetContainer: targetContainer,
		TargetSignature: targetSignature,

		Consumer: comm.NewStateConsumer(),
		Compression: &pwr.CompressionSettings{
			Algorithm: pwr.CompressionAlgorithm_BROTLI,
			Quality:   int32(*diffArgs.quality),
		},
	}

	comm.Opf("Diffing %s", source)
	comm.StartProgress()
	err = dctx.WritePatch(patchCounter, signatureCounter)
	if err != nil {
		return err
	}
	comm.EndProgress()

	{
		prettySize := humanize.Bytes(uint64(sourceContainer.Size))
		perSecond := humanize.Bytes(uint64(float64(sourceContainer.Size) / time.Since(startTime).Seconds()))
		comm.Statf("%s (%s) @ %s/s\n", prettySize, sourceContainer.Stats(), perSecond)
	}

	if *diffArgs.verify {
		tmpDir, err := ioutil.TempDir("", "pwr")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpDir)

		apply(patch, target, tmpDir, false)

		verify(signaturePath, tmpDir)
	}

	{
		prettyPatchSize := humanize.Bytes(uint64(patchCounter.Count()))
		percReused := 100.0 * float64(dctx.ReusedBytes) / float64(dctx.FreshBytes+dctx.ReusedBytes)
		relToNew := 100.0 * float64(patchCounter.Count()) / float64(sourceContainer.Size)
		prettyFreshSize := humanize.Bytes(uint64(dctx.FreshBytes))

		comm.Statf("Re-used %.2f%% of old, added %s fresh data", percReused, prettyFreshSize)
		comm.Statf("%s patch (%.2f%% of the full size)", prettyPatchSize, relToNew)
	}

	return nil
}

func apply(patch string, target string, output string, inplace bool) {
	must(doApply(patch, target, output, inplace))
}

func doApply(patch string, target string, output string, inplace bool) error {
	if output == "" {
		output = target
	}

	target = path.Clean(target)
	output = path.Clean(output)
	if output == target {
		if !inplace {
			comm.Dief("Refusing to destructively patch %s without --inplace", output)
		}
	}

	comm.Opf("Patching %s", output)
	startTime := time.Now()

	patchReader, err := os.Open(patch)
	if err != nil {
		return err
	}

	actx := &pwr.ApplyContext{
		TargetPath: target,
		OutputPath: output,
		InPlace:    inplace,

		Consumer: comm.NewStateConsumer(),
	}

	comm.StartProgress()
	err = actx.ApplyPatch(patchReader)
	if err != nil {
		return err
	}
	comm.EndProgress()

	container := actx.SourceContainer
	prettySize := humanize.Bytes(uint64(container.Size))
	perSecond := humanize.Bytes(uint64(float64(container.Size) / time.Since(startTime).Seconds()))
	comm.Statf("%s (%s) @ %s/s (touched %d files)\n", prettySize, container.Stats(), perSecond, actx.TouchedFiles)

	return nil
}

func sign(output string, signature string) {
	must(doSign(output, signature))
}

func doSign(output string, signature string) error {
	comm.Opf("Creating signature for %s", output)
	startTime := time.Now()

	container, err := tlc.Walk(output, filterPaths)
	if err != nil {
		return err
	}

	signatureWriter, err := os.Create(signature)
	if err != nil {
		return err
	}

	compression := pwr.CompressionDefault()

	rawSigWire := wire.NewWriteContext(signatureWriter)
	rawSigWire.WriteMagic(pwr.SignatureMagic)

	rawSigWire.WriteMessage(&pwr.SignatureHeader{
		Compression: compression,
	})

	sigWire, err := pwr.CompressWire(rawSigWire, compression)
	if err != nil {
		return err
	}
	sigWire.WriteMessage(container)

	comm.StartProgress()
	err = pwr.ComputeSignatureToWriter(container, output, comm.NewStateConsumer(), func(hash sync.BlockHash) error {
		return sigWire.WriteMessage(&pwr.BlockHash{
			WeakHash:   hash.WeakHash,
			StrongHash: hash.StrongHash,
		})
	})
	comm.EndProgress()
	if err != nil {
		return err
	}

	err = sigWire.Close()
	if err != nil {
		return err
	}

	prettySize := humanize.Bytes(uint64(container.Size))
	perSecond := humanize.Bytes(uint64(float64(container.Size) / time.Since(startTime).Seconds()))
	comm.Statf("%s (%s) @ %s/s\n", prettySize, container.Stats(), perSecond)

	return nil
}

func verify(signature string, output string) {
	must(doVerify(signature, output))
}

func doVerify(signature string, output string) error {
	comm.Opf("Verifying %s", output)
	startTime := time.Now()

	signatureReader, err := os.Open(signature)
	if err != nil {
		return err
	}
	defer signatureReader.Close()

	refContainer, refHashes, err := pwr.ReadSignature(signatureReader)
	if err != nil {
		return err
	}

	comm.StartProgress()
	hashes, err := pwr.ComputeSignature(refContainer, output, comm.NewStateConsumer())
	comm.EndProgress()
	if err != nil {
		return err
	}

	err = pwr.CompareHashes(refHashes, hashes)
	if err != nil {
		comm.Logf(err.Error())
		comm.Dief("Some checks failed after checking %d block.", len(refHashes))
	}

	prettySize := humanize.Bytes(uint64(refContainer.Size))
	perSecond := humanize.Bytes(uint64(float64(refContainer.Size) / time.Since(startTime).Seconds()))
	comm.Statf("%s (%s) @ %s/s\n", prettySize, refContainer.Stats(), perSecond)

	return nil
}
