// Copyright (C) 2017 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vulkan

import (
	"context"
	"fmt"

	"github.com/google/gapid/core/log"
	"github.com/google/gapid/core/math/interval"
	"github.com/google/gapid/core/memory/arena"
	"github.com/google/gapid/gapis/api"
	"github.com/google/gapid/gapis/config"
	"github.com/google/gapid/gapis/memory"
	"github.com/google/gapid/gapis/resolve/dependencygraph"
)

var emptyDefUseVars = []dependencygraph.DefUseVariable{}

const vkWholeSize = uint64(0xFFFFFFFFFFFFFFFF)
const vkAttachmentUnused = uint32(0xFFFFFFFF)
const vkRemainingArrayLayers = uint32(0xFFFFFFFF)
const vkRemainingMipLevels = uint32(0xFFFFFFFF)

// Assume the value of a Vulkan handle is always unique
type vkHandle struct {
	handle uint64
	b      *dependencygraph.Behavior
}

func (h *vkHandle) GetDefBehavior() *dependencygraph.Behavior {
	return h.b
}

func (h *vkHandle) SetDefBehavior(b *dependencygraph.Behavior) {
	h.b = b
}

func (h *vkHandle) isNullHandle() bool {
	return h.handle == uint64(0)
}

// label
type label struct {
	uint64
	b *dependencygraph.Behavior
}

func (l *label) GetDefBehavior() *dependencygraph.Behavior {
	return l.b
}

func (l *label) SetDefBehavior(b *dependencygraph.Behavior) {
	l.b = b
}

var nextLabelVal uint64 = 1

func newLabel() *label { i := nextLabelVal; nextLabelVal++; return &label{i, nil} }

// Forward-paired label
type forwardPairedLabel struct {
	labelReadBehaviors []*dependencygraph.Behavior
	b                  *dependencygraph.Behavior
}

func (l *forwardPairedLabel) GetDefBehavior() *dependencygraph.Behavior {
	return l.b
}
func (l *forwardPairedLabel) SetDefBehavior(b *dependencygraph.Behavior) {
	l.b = b
}

func newForwardPairedLabel(ctx context.Context,
	bh *dependencygraph.Behavior) *forwardPairedLabel {
	fpl := &forwardPairedLabel{labelReadBehaviors: []*dependencygraph.Behavior{}, b: nil}
	write(ctx, bh, fpl)
	return fpl
}

// memory
type memorySpan struct {
	sp       interval.U64Span
	memory   VkDeviceMemory
	b        *dependencygraph.Behavior
	recordTo *memorySpanRecords
}

func (s *memorySpan) GetDefBehavior() *dependencygraph.Behavior {
	return s.b
}

func (s *memorySpan) SetDefBehavior(b *dependencygraph.Behavior) {
	s.b = b
}

func (s *memorySpan) size() uint64 {
	return s.sp.End - s.sp.Start
}

func (s *memorySpan) span() interval.U64Span {
	return s.sp
}

func (s *memorySpan) shrink(offset, size uint64) error {
	if size == vkWholeSize {
		size = s.size() - offset
	}
	if (offset+size < offset) || (offset+size > s.size()) {
		return shrinkOutOfMemBindingBound{s, offset, size}
	}
	s.sp.Start += offset
	s.sp.End = s.sp.Start + size
	return nil
}

func (s *memorySpan) duplicate() memBinding {
	newS := *s
	return &newS
}

type memorySpanList memBindingList

// commands
type commandBufferCommand struct {
	isCmdExecuteCommands    bool
	secondaryCommandBuffers []VkCommandBuffer
	behave                  func(submittedCommand, *queueExecutionState)
	b                       *dependencygraph.Behavior
}

func (cbc *commandBufferCommand) newBehavior(ctx context.Context,
	sc submittedCommand, qei *queueExecutionState) *dependencygraph.Behavior {
	bh := dependencygraph.NewBehavior(sc.id)
	read(ctx, bh, cbc)
	read(ctx, bh, qei.currentSubmitInfo.queued)
	if sc.parentCmd != nil {
		read(ctx, bh, sc.parentCmd)
	}
	return bh
}

func (cbc *commandBufferCommand) GetDefBehavior() *dependencygraph.Behavior {
	return cbc.b
}

func (cbc *commandBufferCommand) SetDefBehavior(b *dependencygraph.Behavior) {
	cbc.b = b
}

func (cbc *commandBufferCommand) recordSecondaryCommandBuffer(vkCb VkCommandBuffer) {
	cbc.secondaryCommandBuffers = append(cbc.secondaryCommandBuffers, vkCb)
}

// submittedCommand represents Subcommands. When a submidttedCommand/Subcommand
// is executed, it reads the original commands, and if it is secondary command,
// its parent primary command.
type submittedCommand struct {
	id        api.SubCmdIdx
	cmd       *commandBufferCommand
	parentCmd *commandBufferCommand
}

func newSubmittedCommand(fullCmdIndex api.SubCmdIdx,
	c *commandBufferCommand, pc *commandBufferCommand) *submittedCommand {
	return &submittedCommand{id: fullCmdIndex, cmd: c, parentCmd: pc}
}

func (sc *submittedCommand) runCommand(ctx context.Context,
	ft *dependencygraph.Footprint, execInfo *queueExecutionState) {
	sc.cmd.behave(*sc, execInfo)
}

type queueSubmitInfo struct {
	queue            VkQueue
	began            bool
	queued           *label
	done             *label
	waitSemaphores   []VkSemaphore
	signalSemaphores []VkSemaphore
	signalFence      VkFence
	pendingCommands  []*submittedCommand
}

type event struct {
	signal   *label
	unsignal *label
}

type fence struct {
	signal   *label
	unsignal *label
}

type query struct {
	reset  *label
	begin  *label
	end    *label
	result *label
}

func newQuery() *query {
	return &query{
		reset:  newLabel(),
		begin:  newLabel(),
		end:    newLabel(),
		result: newLabel(),
	}
}

type queryPool struct {
	queries []*query
}

type subpassAttachmentInfo struct {
	fullImageData bool
	data          []dependencygraph.DefUseVariable
	layout        *label
	desc          VkAttachmentDescription
}

type subpassInfo struct {
	loadAttachments        []*subpassAttachmentInfo
	storeAttachments       []*subpassAttachmentInfo
	colorAttachments       []*subpassAttachmentInfo
	resolveAttachments     []*subpassAttachmentInfo
	inputAttachments       []*subpassAttachmentInfo
	depthStencilAttachment *subpassAttachmentInfo
	modifiedDescriptorData []dependencygraph.DefUseVariable
}

type subpassIndex struct {
	val uint64
	b   *dependencygraph.Behavior
}

func (si *subpassIndex) GetDefBehavior() *dependencygraph.Behavior {
	return si.b
}
func (si *subpassIndex) SetDefBehavior(b *dependencygraph.Behavior) {
	si.b = b
}

type commandBufferExecutionState struct {
	vertexBufferResBindings map[uint32]resBindingList
	indexBufferResBindings  resBindingList
	indexType               VkIndexType
	descriptorSets          map[uint32]*boundDescriptorSet
	pipeline                *label
	dynamicState            *label
}

func newCommandBufferExecutionState() *commandBufferExecutionState {
	return &commandBufferExecutionState{
		vertexBufferResBindings: map[uint32]resBindingList{},
		descriptorSets:          map[uint32]*boundDescriptorSet{},
		pipeline:                newLabel(),
		dynamicState:            newLabel(),
	}
}

type queueExecutionState struct {
	currentCmdBufState   *commandBufferExecutionState
	primaryCmdBufState   *commandBufferExecutionState
	secondaryCmdBufState *commandBufferExecutionState

	subpasses       []subpassInfo
	subpass         *subpassIndex
	renderPassBegin *forwardPairedLabel

	currentCommand api.SubCmdIdx

	framebuffer FramebufferObjectʳ

	lastSubmitID      api.CmdID
	currentSubmitInfo *queueSubmitInfo
}

func newQueueExecutionState(id api.CmdID) *queueExecutionState {
	return &queueExecutionState{
		subpasses:      []subpassInfo{},
		lastSubmitID:   id,
		currentCommand: api.SubCmdIdx([]uint64{0, 0, 0, 0}),
	}
}

func (qei *queueExecutionState) updateCurrentCommand(ctx context.Context,
	fci api.SubCmdIdx) {
	switch len(fci) {
	case 4:
		current := api.SubCmdIdx(qei.currentCommand[0:3])
		comming := api.SubCmdIdx(fci[0:3])
		if current.LessThan(comming) {
			// primary command buffer changed
			qei.primaryCmdBufState = newCommandBufferExecutionState()
		}
		qei.currentCmdBufState = qei.primaryCmdBufState
	case 6:
		if len(qei.currentCommand) != 6 {
			// Transit from primary command buffer to secondary command buffer
			qei.secondaryCmdBufState = newCommandBufferExecutionState()
		} else {
			current := api.SubCmdIdx(qei.currentCommand[0:5])
			comming := api.SubCmdIdx(fci[0:5])
			if current.LessThan(comming) {
				// secondary command buffer changed
				qei.secondaryCmdBufState = newCommandBufferExecutionState()
			}
		}
		qei.currentCmdBufState = qei.secondaryCmdBufState
	default:
		log.E(ctx, "FootprintBuilder: Invalid length of full command index")
	}
	qei.currentCommand = fci
}

func (o VkAttachmentLoadOp) isLoad() bool {
	return o == VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD
}

func (o VkAttachmentStoreOp) isStore() bool {
	return o == VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_STORE
}

func (qei *queueExecutionState) startSubpass(ctx context.Context,
	bh *dependencygraph.Behavior) {
	write(ctx, bh, qei.subpass)
	subpassI := qei.subpass.val
	noDsAttLoadOp := func(ctx context.Context, bh *dependencygraph.Behavior,
		attachment *subpassAttachmentInfo) {
		// TODO: Not all subpasses change layouts
		modify(ctx, bh, attachment.layout)
		if attachment.desc.LoadOp().isLoad() {
			read(ctx, bh, attachment.data...)
		} else {
			if attachment.fullImageData {
				write(ctx, bh, attachment.data...)
			} else {
				modify(ctx, bh, attachment.data...)
			}
		}
	}
	dsAttLoadOp := func(ctx context.Context, bh *dependencygraph.Behavior,
		attachment *subpassAttachmentInfo) {
		// TODO: Not all subpasses change layouts
		modify(ctx, bh, attachment.layout)
		if !attachment.desc.LoadOp().isLoad() && !attachment.desc.StencilLoadOp().isLoad() {
			if attachment.fullImageData {
				write(ctx, bh, attachment.data...)
			} else {
				modify(ctx, bh, attachment.data...)
			}
		} else if attachment.desc.LoadOp().isLoad() && attachment.desc.StencilLoadOp().isLoad() {
			read(ctx, bh, attachment.data...)
		} else {
			modify(ctx, bh, attachment.data...)
		}
	}
	for _, l := range qei.subpasses[subpassI].loadAttachments {
		if qei.subpasses[subpassI].depthStencilAttachment == l {
			dsAttLoadOp(ctx, bh, l)
		} else {
			noDsAttLoadOp(ctx, bh, l)
		}
	}
}

func (qei *queueExecutionState) emitSubpassOutput(ctx context.Context,
	ft *dependencygraph.Footprint, sc submittedCommand) {
	subpassI := qei.subpass.val
	noDsAttStoreOp := func(ctx context.Context, ft *dependencygraph.Footprint,
		sc submittedCommand, att *subpassAttachmentInfo,
		readAtt *subpassAttachmentInfo) {
		// Two behaviors for each attachment. One to represent the dependency of
		// image layout, another one for the data.
		behaviorForLayout := sc.cmd.newBehavior(ctx, sc, qei)
		modify(ctx, behaviorForLayout, att.layout)
		read(ctx, behaviorForLayout, qei.subpass)
		ft.AddBehavior(ctx, behaviorForLayout)

		behaviorForData := sc.cmd.newBehavior(ctx, sc, qei)
		if readAtt != nil {
			read(ctx, behaviorForData, readAtt.data...)
		}
		if att.desc.StoreOp().isStore() {
			modify(ctx, behaviorForData, att.data...)
		} else {
			// If the attachment fully covers the unlying image, this will clear
			// the image data, which is a write operation.
			if att.fullImageData {
				write(ctx, behaviorForData, att.data...)
			} else {
				modify(ctx, behaviorForData, att.data...)
			}
		}
		read(ctx, behaviorForData, qei.subpass)
		ft.AddBehavior(ctx, behaviorForData)
	}

	dsAttStoreOp := func(ctx context.Context, ft *dependencygraph.Footprint,
		sc submittedCommand, dsAtt *subpassAttachmentInfo) {
		bh := sc.cmd.newBehavior(ctx, sc, qei)
		if dsAtt.desc.StoreOp().isStore() || dsAtt.desc.StencilStoreOp().isStore() {
			modify(ctx, bh, dsAtt.data...)
		} else {
			if dsAtt.fullImageData {
				write(ctx, bh, dsAtt.data...)
			} else {
				modify(ctx, bh, dsAtt.data...)
			}
		}
		read(ctx, bh, qei.subpass)
		ft.AddBehavior(ctx, bh)
	}

	isStoreAtt := func(att *subpassAttachmentInfo) bool {
		for _, storeAtt := range qei.subpasses[subpassI].storeAttachments {
			if att == storeAtt {
				return true
			}
		}
		return false
	}

	for i, r := range qei.subpasses[subpassI].resolveAttachments {
		if isStoreAtt(r) {
			c := qei.subpasses[subpassI].colorAttachments[i]
			noDsAttStoreOp(ctx, ft, sc, r, c)
		}
	}
	for _, c := range qei.subpasses[subpassI].colorAttachments {
		if isStoreAtt(c) {
			noDsAttStoreOp(ctx, ft, sc, c, nil)
		}
	}
	for _, i := range qei.subpasses[subpassI].inputAttachments {
		if isStoreAtt(i) {
			noDsAttStoreOp(ctx, ft, sc, i, nil)
		}
	}
	if isStoreAtt(qei.subpasses[subpassI].depthStencilAttachment) {
		dsAttStoreOp(ctx, ft, sc, qei.subpasses[subpassI].depthStencilAttachment)
	}
	for _, modified := range qei.subpasses[subpassI].modifiedDescriptorData {
		bh := sc.cmd.newBehavior(ctx, sc, qei)
		modify(ctx, bh, modified)
		read(ctx, bh, qei.subpass)
		ft.AddBehavior(ctx, bh)
	}
}

func (qei *queueExecutionState) endSubpass(ctx context.Context,
	ft *dependencygraph.Footprint, bh *dependencygraph.Behavior,
	sc submittedCommand) {
	qei.emitSubpassOutput(ctx, ft, sc)
	read(ctx, bh, qei.subpass)
}

func (qei *queueExecutionState) beginRenderPass(ctx context.Context,
	vb *FootprintBuilder, bh *dependencygraph.Behavior,
	rp RenderPassObjectʳ, fb FramebufferObjectʳ) {
	read(ctx, bh, vb.toVkHandle(uint64(rp.VulkanHandle())))
	read(ctx, bh, vb.toVkHandle(uint64(fb.VulkanHandle())))
	qei.framebuffer = fb
	qei.subpasses = make([]subpassInfo, 0, rp.SubpassDescriptions().Len())

	// Record which subpass that loads or stores the attachments. A subpass loads
	// an attachment if the attachment is first used in that subpass. A subpass
	// stores an attachment if the subpass is the last use of that attachment.
	attLoadSubpass := make(map[uint32]uint32, fb.ImageAttachments().Len())
	attStoreSubpass := make(map[uint32]uint32, fb.ImageAttachments().Len())
	attStoreAttInfo := make(map[uint32]*subpassAttachmentInfo, fb.ImageAttachments().Len())
	recordAttachment := func(ai, si uint32) *subpassAttachmentInfo {
		viewObj := fb.ImageAttachments().Get(ai)
		imgObj := viewObj.Image()
		imgLayout, imgData := vb.getImageLayoutAndData(ctx, bh, imgObj.VulkanHandle())
		attDesc := rp.AttachmentDescriptions().Get(ai)
		fullImageData := false
		switch viewObj.Type() {
		case VkImageViewType_VK_IMAGE_VIEW_TYPE_2D,
			VkImageViewType_VK_IMAGE_VIEW_TYPE_2D_ARRAY:
			if viewObj.SubresourceRange().BaseArrayLayer() == uint32(0) &&
				(imgObj.Info().ArrayLayers() == viewObj.SubresourceRange().LayerCount() ||
					viewObj.SubresourceRange().LayerCount() == vkRemainingArrayLayers) &&
				imgObj.Info().ImageType() == VkImageType_VK_IMAGE_TYPE_2D &&
				imgObj.Info().Extent().Width() == fb.Width() &&
				imgObj.Info().Extent().Height() == fb.Height() &&
				(fb.Layers() == imgObj.Info().ArrayLayers() ||
					fb.Layers() == vkRemainingArrayLayers) {
				fullImageData = true
			}
		}
		attachmentInfo := &subpassAttachmentInfo{fullImageData, imgData, imgLayout, attDesc}
		if _, ok := attLoadSubpass[ai]; !ok {
			attLoadSubpass[ai] = si
			qei.subpasses[si].loadAttachments = append(
				qei.subpasses[si].loadAttachments, attachmentInfo)
		}
		attStoreSubpass[ai] = si
		attStoreAttInfo[ai] = attachmentInfo
		return attachmentInfo
	}
	defer func() {
		for ai, si := range attStoreSubpass {
			qei.subpasses[si].storeAttachments = append(
				qei.subpasses[si].storeAttachments, attStoreAttInfo[ai])
		}
	}()

	for _, subpass := range rp.SubpassDescriptions().Keys() {
		desc := rp.SubpassDescriptions().Get(subpass)
		colorAs := make(map[uint32]struct{}, desc.ColorAttachments().Len())
		resolveAs := make(map[uint32]struct{}, desc.ResolveAttachments().Len())
		inputAs := make(map[uint32]struct{}, desc.InputAttachments().Len())

		for _, ref := range desc.ColorAttachments().All() {
			if ref.Attachment() != vkAttachmentUnused {
				colorAs[ref.Attachment()] = struct{}{}
			}
		}
		for _, ref := range desc.ResolveAttachments().All() {
			if ref.Attachment() != vkAttachmentUnused {
				resolveAs[ref.Attachment()] = struct{}{}
			}
		}
		for _, ref := range desc.InputAttachments().All() {
			if ref.Attachment() != vkAttachmentUnused {
				inputAs[ref.Attachment()] = struct{}{}
			}
		}
		qei.subpasses = append(qei.subpasses, subpassInfo{
			colorAttachments:   make([]*subpassAttachmentInfo, 0, len(colorAs)),
			resolveAttachments: make([]*subpassAttachmentInfo, 0, len(resolveAs)),
			inputAttachments:   make([]*subpassAttachmentInfo, 0, len(inputAs)),
		})
		if subpass != uint32(len(qei.subpasses)-1) {
			log.E(ctx, "FootprintBuilder: Cannot get subpass info, subpass: %v, length of info: %v",
				subpass, uint32(len(qei.subpasses)))
		}
		// TODO: handle preserveAttachments

		for _, viewObj := range fb.ImageAttachments().All() {
			if read(ctx, bh, vb.toVkHandle(uint64(viewObj.VulkanHandle()))) {
				read(ctx, bh, vb.toVkHandle(uint64(viewObj.Image().VulkanHandle())))
			}
		}

		for _, ai := range rp.AttachmentDescriptions().Keys() {
			if _, ok := colorAs[ai]; ok {
				qei.subpasses[subpass].colorAttachments = append(
					qei.subpasses[subpass].colorAttachments,
					recordAttachment(ai, subpass))
			}
			if _, ok := resolveAs[ai]; ok {
				qei.subpasses[subpass].resolveAttachments = append(
					qei.subpasses[subpass].resolveAttachments,
					recordAttachment(ai, subpass))
			}
			if _, ok := inputAs[ai]; ok {
				qei.subpasses[subpass].inputAttachments = append(
					qei.subpasses[subpass].inputAttachments,
					recordAttachment(ai, subpass))
			}
		}
		if !desc.DepthStencilAttachment().IsNil() {
			dsAi := desc.DepthStencilAttachment().Attachment()
			if dsAi != vkAttachmentUnused {
				viewObj := fb.ImageAttachments().Get(dsAi)
				imgObj := viewObj.Image()
				imgLayout, imgData := vb.getImageLayoutAndData(ctx, bh, imgObj.VulkanHandle())
				attDesc := rp.AttachmentDescriptions().Get(dsAi)
				fullImageData := false
				switch viewObj.Type() {
				case VkImageViewType_VK_IMAGE_VIEW_TYPE_2D,
					VkImageViewType_VK_IMAGE_VIEW_TYPE_2D_ARRAY:
					if viewObj.SubresourceRange().BaseArrayLayer() == uint32(0) &&
						(imgObj.Info().ArrayLayers() == viewObj.SubresourceRange().LayerCount() ||
							viewObj.SubresourceRange().LayerCount() == vkRemainingMipLevels) &&
						imgObj.Info().ImageType() == VkImageType_VK_IMAGE_TYPE_2D &&
						imgObj.Info().Extent().Width() == fb.Width() &&
						imgObj.Info().Extent().Height() == fb.Height() &&
						(fb.Layers() == imgObj.Info().ArrayLayers() ||
							fb.Layers() == vkRemainingArrayLayers) {
						fullImageData = true
					}
				}
				qei.subpasses[subpass].depthStencilAttachment = &subpassAttachmentInfo{
					fullImageData, imgData, imgLayout, attDesc}
			}
		}
	}
	qei.subpass = &subpassIndex{0, nil}
	qei.startSubpass(ctx, bh)
}

func (qei *queueExecutionState) nextSubpass(ctx context.Context,
	ft *dependencygraph.Footprint, bh *dependencygraph.Behavior,
	sc submittedCommand) {
	qei.endSubpass(ctx, ft, bh, sc)
	qei.subpass.val++
	qei.startSubpass(ctx, bh)
}

func (qei *queueExecutionState) endRenderPass(ctx context.Context,
	ft *dependencygraph.Footprint, bh *dependencygraph.Behavior,
	sc submittedCommand) {
	qei.endSubpass(ctx, ft, bh, sc)
}

type renderpass struct {
	begin *label
	end   *label
}

type commandBuffer struct {
	begin           *label
	end             *label
	renderPassBegin *label
}

type resBinding struct {
	resourceOffset uint64
	bindSize       uint64
	backingData    dependencygraph.DefUseVariable
	b              *dependencygraph.Behavior
}

// resBinding implements interface memBinding
func (bd *resBinding) size() uint64 {
	return bd.bindSize
}

func (bd *resBinding) span() interval.U64Span {
	return interval.U64Span{Start: bd.resourceOffset, End: bd.resourceOffset + bd.size()}
}

func (bd *resBinding) shrink(offset, size uint64) error {
	if size == vkWholeSize {
		size = bd.size() - offset
	}
	if (offset+size < offset) || (offset+size > bd.size()) {
		return shrinkOutOfMemBindingBound{bd, offset, size}
	}
	if sp, isSpan := bd.backingData.(*memorySpan); isSpan {
		bd.bindSize = size
		bd.resourceOffset += offset
		sp.sp.Start += offset
		sp.sp.End = sp.sp.Start + size
		bd.backingData = sp
		return nil
	}
	if offset != 0 || size != bd.size() {
		return fmt.Errorf("Cannot shrink a resBinding whose backing data is not a memorySpan into different size and offset than the original one")
	}
	return nil
}

func (bd *resBinding) duplicate() memBinding {
	newB := *bd
	if d, ok := bd.backingData.(*memorySpan); ok {
		newB.backingData = d.duplicate().(*memorySpan)
	}
	return &newB
}

func newResBinding(ctx context.Context, bh *dependencygraph.Behavior,
	resOffset, size uint64, res dependencygraph.DefUseVariable) *resBinding {
	d := &resBinding{resourceOffset: resOffset, bindSize: size, backingData: res}
	if bh != nil {
		write(ctx, bh, d)
	}
	return d
}

func newSpanResBinding(ctx context.Context, vb *FootprintBuilder, bh *dependencygraph.Behavior,
	memory VkDeviceMemory, resOffset, size, memoryOffset uint64) *resBinding {
	return newResBinding(ctx, bh, resOffset, size, vb.newMemorySpan(memory, memoryOffset, size))
}

func newNonSpanResBinding(ctx context.Context, bh *dependencygraph.Behavior,
	size uint64) *resBinding {
	return newResBinding(ctx, bh, 0, size, newLabel())
}

func (bd *resBinding) newSubBinding(ctx context.Context,
	bh *dependencygraph.Behavior, offset, size uint64) (*resBinding, error) {
	subBinding, _ := bd.duplicate().(*resBinding)
	if err := subBinding.shrink(offset, size); err != nil {
		return nil, err
	}
	if bh != nil {
		write(ctx, bh, subBinding)
	}
	return subBinding, nil
}

func (bd *resBinding) GetDefBehavior() *dependencygraph.Behavior {
	return bd.b
}
func (bd *resBinding) SetDefBehavior(b *dependencygraph.Behavior) {
	bd.b = b
}

// Implements the interval.List interface for resBinding slices
type resBindingList memBindingList

func (rl resBindingList) resBindings() []*resBinding {
	ret := make([]*resBinding, 0, len(rl))
	for _, b := range rl {
		if rb, ok := b.(*resBinding); ok {
			ret = append(ret, rb)
		}
	}
	return ret
}

func addResBinding(ctx context.Context, l resBindingList, b *resBinding) resBindingList {
	var err error
	ml := memBindingList(l)
	ml, err = addBinding(ml, b)
	if err != nil {
		log.E(ctx, "FootprintBuilder: %s", err.Error())
		return l
	}
	return resBindingList(ml)
}

func (l resBindingList) getSubBindingList(ctx context.Context,
	bh *dependencygraph.Behavior, offset, size uint64) resBindingList {
	subBindings := resBindingList{}
	if offset+size < offset {
		// overflow
		size = vkWholeSize - offset
	}
	first, count := interval.Intersect(memBindingList(l),
		interval.U64Span{Start: offset, End: offset + size})
	if count == 0 {
		return subBindings
	} else {
		bl := l.resBindings()
		for i := first; i < first+count; i++ {
			start := bl[i].span().Start
			end := bl[i].span().End
			if offset > start {
				start = offset
			}
			if offset+size < end {
				end = offset + size
			}
			if bh != nil {
				read(ctx, bh, bl[i])
			}
			newB, err := bl[i].newSubBinding(ctx, bh, start-bl[i].span().Start, end-start)
			if err != nil {
				log.E(ctx, "FootprintBuilder: %s", err.Error())
			}
			if newB != nil {
				subBindings = append(subBindings, newB)
			}
		}
	}
	return subBindings
}

func (l resBindingList) getBoundData(ctx context.Context,
	bh *dependencygraph.Behavior, offset, size uint64) []dependencygraph.DefUseVariable {
	data := []dependencygraph.DefUseVariable{}
	bindingList := l.getSubBindingList(ctx, bh, offset, size)
	for _, b := range bindingList.resBindings() {
		if b == nil {
			// skip invalid bindings
			continue
		}
		data = append(data, b.backingData)
	}
	return data
}

type descriptor struct {
	ty VkDescriptorType
	// for image descriptor
	img VkImage
	// only used for sampler and sampler combined descriptors
	sampler *vkHandle
	// for buffer descriptor
	buf       VkBuffer
	bufOffset VkDeviceSize
	bufRng    VkDeviceSize
	// the behavior that defines(aka. writes) this descriptor
	b *dependencygraph.Behavior
}

func (dp *descriptor) GetDefBehavior() *dependencygraph.Behavior {
	return dp.b
}
func (dp *descriptor) SetDefBehavior(b *dependencygraph.Behavior) {
	dp.b = b
}

type descriptorSet struct {
	descriptors            api.SubCmdIdxTrie
	descriptorCounts       map[uint64]uint64 // binding -> descriptor count of that binding
	dynamicDescriptorCount uint64
}

func newDescriptorSet() *descriptorSet {
	return &descriptorSet{
		descriptors:            api.SubCmdIdxTrie{},
		descriptorCounts:       map[uint64]uint64{},
		dynamicDescriptorCount: uint64(0),
	}
}

func (ds *descriptorSet) reserveDescriptor(bi, di uint64) {
	if _, ok := ds.descriptorCounts[bi]; !ok {
		ds.descriptorCounts[bi] = uint64(0)
	}
	ds.descriptorCounts[bi]++
}

func (ds *descriptorSet) getDescriptor(ctx context.Context,
	bh *dependencygraph.Behavior, bi, di uint64) *descriptor {
	if v := ds.descriptors.Value([]uint64{bi, di}); v != nil {
		if d, ok := v.(*descriptor); ok {
			read(ctx, bh, d)
			return d
		}
		log.E(ctx, "FootprintBuilder: Not *descriptor type in descriptorSet: %v, with "+
			"binding: %v, array index: %v", *ds, bi, di)
		return nil
	}
	return nil
}

func (ds *descriptorSet) setDescriptor(ctx context.Context,
	bh *dependencygraph.Behavior, bi, di uint64, ty VkDescriptorType,
	vkImg VkImage, sampler *vkHandle, vkBuf VkBuffer, boundOffset, rng VkDeviceSize) {
	if v := ds.descriptors.Value([]uint64{bi, di}); v != nil {
		if d, ok := v.(*descriptor); ok {
			if d.ty == VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_BUFFER_DYNAMIC ||
				d.ty == VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER_DYNAMIC {
				ds.dynamicDescriptorCount--
			}
		} else {
			log.E(ctx, "FootprintBuilder: Not *descriptor type in descriptorSet: %v, with "+
				"binding: %v, array index: %v", *ds, bi, di)
		}
	}
	d := &descriptor{ty: ty, img: vkImg, sampler: sampler, buf: vkBuf, bufOffset: boundOffset, bufRng: rng}
	ds.descriptors.SetValue([]uint64{bi, di}, d)
    write(ctx, bh, d)
	if ty == VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_BUFFER_DYNAMIC ||
		ty == VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER_DYNAMIC {
		ds.dynamicDescriptorCount++
	}
}

func (ds *descriptorSet) useDescriptors(ctx context.Context, vb *FootprintBuilder,
	bh *dependencygraph.Behavior, dynamicOffsets []uint32) []dependencygraph.DefUseVariable {
	modified := []dependencygraph.DefUseVariable{}
	doi := 0
	for binding, count := range ds.descriptorCounts {
		for di := uint64(0); di < count; di++ {
			d := ds.getDescriptor(ctx, bh, binding, di)
			if d != nil {
				read(ctx, bh, d.sampler)
				switch d.ty {
				case VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_IMAGE:
					data := vb.getImageData(ctx, bh, d.img)
					modify(ctx, bh, data...)
					modified = append(modified, data...)
				case VkDescriptorType_VK_DESCRIPTOR_TYPE_SAMPLER:
					// pass, as the sampler has been 'read' before the switch
				case VkDescriptorType_VK_DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER,
					VkDescriptorType_VK_DESCRIPTOR_TYPE_SAMPLED_IMAGE,
					VkDescriptorType_VK_DESCRIPTOR_TYPE_INPUT_ATTACHMENT:
					data := vb.getImageData(ctx, bh, d.img)
					read(ctx, bh, data...)
				case VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
					VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_TEXEL_BUFFER:
					data := vb.getBufferData(ctx, bh, d.buf, uint64(d.bufOffset), uint64(d.bufRng))
					modify(ctx, bh, data...)
					modified = append(modified, data...)
				case VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_BUFFER_DYNAMIC:
					if doi < len(dynamicOffsets) {
						data := vb.getBufferData(ctx, bh, d.buf,
							uint64(dynamicOffsets[doi])+uint64(d.bufOffset), uint64(d.bufRng))
						doi++
						modify(ctx, bh, data...)
						modified = append(modified, data...)
					} else {
						log.E(ctx, "FootprintBuilder: DescriptorSet: %v has more dynamic descriptors than reserved dynamic offsets", *ds)
					}
				case VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER,
					VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_TEXEL_BUFFER:
					data := vb.getBufferData(ctx, bh, d.buf, uint64(d.bufOffset), uint64(d.bufRng))
					read(ctx, bh, data...)
				case VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER_DYNAMIC:
					if doi < len(dynamicOffsets) {
						data := vb.getBufferData(ctx, bh, d.buf,
							uint64(dynamicOffsets[doi])+uint64(d.bufOffset), uint64(d.bufRng))
						doi++
						read(ctx, bh, data...)
					} else {
						log.E(ctx, "FootprintBuilder: DescriptorSet: %v has more dynamic descriptors than reserved dynamic offsets", *ds)
					}
				}
			}
		}
	}
	return modified
}

func (ds *descriptorSet) writeDescriptors(ctx context.Context,
	cmd api.Cmd, s *api.GlobalState, vb *FootprintBuilder,
	bh *dependencygraph.Behavior,
	write VkWriteDescriptorSet) {
	l := s.MemoryLayout
	dstElm := uint64(write.DstArrayElement())
	count := uint64(write.DescriptorCount())
	dstBinding := uint64(write.DstBinding())
	updateDstForOverflow := func() {
		if dstElm >= ds.descriptorCounts[dstBinding] {
			dstBinding++
			dstElm = uint64(0)
		}
	}
	switch write.DescriptorType() {
	case VkDescriptorType_VK_DESCRIPTOR_TYPE_SAMPLER,
		VkDescriptorType_VK_DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER,
		VkDescriptorType_VK_DESCRIPTOR_TYPE_SAMPLED_IMAGE,
		VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_IMAGE,
		VkDescriptorType_VK_DESCRIPTOR_TYPE_INPUT_ATTACHMENT:
		for _, imageInfo := range write.PImageInfo().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			updateDstForOverflow()
			sampler := vb.toVkHandle(0)
			vkImg := VkImage(0)
			if write.DescriptorType() != VkDescriptorType_VK_DESCRIPTOR_TYPE_SAMPLER &&
				read(ctx, bh, vb.toVkHandle(uint64(imageInfo.ImageView()))) {
				vkView := imageInfo.ImageView()
				vkImg = GetState(s).ImageViews().Get(vkView).Image().VulkanHandle()
			}
			if (write.DescriptorType() == VkDescriptorType_VK_DESCRIPTOR_TYPE_SAMPLER ||
				write.DescriptorType() == VkDescriptorType_VK_DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER) &&
				read(ctx, bh, vb.toVkHandle(uint64(imageInfo.Sampler()))) {
				sampler = vb.toVkHandle(uint64(imageInfo.Sampler()))
			}
			ds.setDescriptor(ctx, bh, dstBinding, dstElm, write.DescriptorType(),
				vkImg, sampler, VkBuffer(0), 0, 0)
			dstElm++
		}
	case VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER,
		VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_BUFFER:
		for _, bufferInfo := range write.PBufferInfo().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			updateDstForOverflow()
			vkBuf := bufferInfo.Buffer()
			read(ctx, bh, vb.toVkHandle(uint64(vkBuf)))
			vb.buffers[vkBuf].getSubBindingList(ctx, bh, uint64(bufferInfo.Offset()), uint64(bufferInfo.Range()))
			ds.setDescriptor(ctx, bh, dstBinding, dstElm, write.DescriptorType(), VkImage(0),
				vb.toVkHandle(0), vkBuf, bufferInfo.Offset(), bufferInfo.Range())
			dstElm++
		}
	case VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_BUFFER_DYNAMIC,
		VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER_DYNAMIC:
		for _, bufferInfo := range write.PBufferInfo().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			updateDstForOverflow()
			vkBuf := bufferInfo.Buffer()
			read(ctx, bh, vb.toVkHandle(uint64(vkBuf)))
			vb.buffers[vkBuf].getSubBindingList(ctx, bh, uint64(bufferInfo.Offset()), uint64(bufferInfo.Range()))
			ds.setDescriptor(ctx, bh, dstBinding, dstElm, write.DescriptorType(), VkImage(0),
				vb.toVkHandle(0), vkBuf, bufferInfo.Offset(), bufferInfo.Range())
			dstElm++
		}
	case VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_TEXEL_BUFFER,
		VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_TEXEL_BUFFER:
		for _, vkBufView := range write.PTexelBufferView().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			updateDstForOverflow()
			read(ctx, bh, vb.toVkHandle(uint64(vkBufView)))
			bufView := GetState(s).BufferViews().Get(vkBufView)
			vkBuf := GetState(s).BufferViews().Get(vkBufView).Buffer().VulkanHandle()
			ds.setDescriptor(ctx, bh, dstBinding, dstElm, write.DescriptorType(),
				VkImage(0), vb.toVkHandle(0), vkBuf, bufView.Offset(), bufView.Range())
			dstElm++
		}
	}
}

func (ds *descriptorSet) copyDescriptors(ctx context.Context,
	cmd api.Cmd, s *api.GlobalState, bh *dependencygraph.Behavior,
	srcDs *descriptorSet, copy VkCopyDescriptorSet) {
	dstElm := uint64(copy.DstArrayElement())
	srcElm := uint64(copy.SrcArrayElement())
	dstBinding := uint64(copy.DstBinding())
	srcBinding := uint64(copy.SrcBinding())
	updateDstAndSrcForOverflow := func() {
		if dstElm >= ds.descriptorCounts[dstBinding] {
			dstBinding++
			dstElm = uint64(0)
		}
		if srcElm >= srcDs.descriptorCounts[srcBinding] {
			srcBinding++
			srcElm = uint64(0)
		}
	}
	for i := uint64(0); i < uint64(copy.DescriptorCount()); i++ {
		updateDstAndSrcForOverflow()
		srcD := srcDs.getDescriptor(ctx, bh, srcBinding, srcElm)
		if srcD != nil {
			ds.setDescriptor(ctx, bh, dstBinding, dstElm, srcD.ty,
				srcD.img, srcD.sampler, srcD.buf, srcD.bufOffset, srcD.bufRng)
		}
		srcElm++
		dstElm++
	}
}

type boundDescriptorSet struct {
	descriptorSet  *descriptorSet
	dynamicOffsets []uint32
	b              *dependencygraph.Behavior
}

func newBoundDescriptorSet(ctx context.Context, bh *dependencygraph.Behavior,
	ds *descriptorSet, dynamicOffsets []uint32) *boundDescriptorSet {
	bds := &boundDescriptorSet{descriptorSet: ds}
	bds.dynamicOffsets = make([]uint32, ds.dynamicDescriptorCount)
	dOffsetCount := len(dynamicOffsets)
	if len(bds.dynamicOffsets) < dOffsetCount {
		dOffsetCount = len(bds.dynamicOffsets)
	}
	for i := 0; i < dOffsetCount; i++ {
		bds.dynamicOffsets[i] = dynamicOffsets[i]
	}
	write(ctx, bh, bds)
	return bds
}

func (bds *boundDescriptorSet) GetDefBehavior() *dependencygraph.Behavior {
	return bds.b
}
func (bds *boundDescriptorSet) SetDefBehavior(b *dependencygraph.Behavior) {
	bds.b = b
}

type sparseImageMemoryBinding struct {
	backingData dependencygraph.DefUseVariable
	b           *dependencygraph.Behavior
}

func newSparseImageMemoryBinding(ctx context.Context, vb *FootprintBuilder,
	bh *dependencygraph.Behavior, memory VkDeviceMemory,
	memoryOffset, size uint64) *sparseImageMemoryBinding {
	b := &sparseImageMemoryBinding{backingData: vb.newMemorySpan(memory, memoryOffset, size)}
	write(ctx, bh, b)
	return b
}

func (simb *sparseImageMemoryBinding) GetDefBehavior() *dependencygraph.Behavior {
	return simb.b
}
func (simb *sparseImageMemoryBinding) SetDefBehavior(b *dependencygraph.Behavior) {
	simb.b = b
}

type imageLayoutAndData struct {
	layout     *label
	opaqueData resBindingList
	sparseData map[VkImageAspectFlags]map[uint32]map[uint32]map[uint64]*sparseImageMemoryBinding
}

func newImageLayoutAndData(ctx context.Context,
	bh *dependencygraph.Behavior) *imageLayoutAndData {
	d := &imageLayoutAndData{layout: newLabel()}
	d.sparseData = map[VkImageAspectFlags]map[uint32]map[uint32]map[uint64]*sparseImageMemoryBinding{}
	write(ctx, bh, d.layout)
	return d
}

type memorySpanRecords struct {
	records map[VkDeviceMemory]memorySpanList
}

// FootprintBuilder implements the FootprintBuilder interface and builds
// Footprint for Vulkan commands.
type FootprintBuilder struct {
	// handles
	handles map[uint64]*vkHandle

	// commands
	commands map[VkCommandBuffer][]*commandBufferCommand

	// coherent memory mapping
	mappedCoherentMemories map[VkDeviceMemory]DeviceMemoryObjectʳ

	// Vulkan handle states
	semaphoreSignals map[VkSemaphore]*label
	fences           map[VkFence]*fence
	events           map[VkEvent]*event
	querypools       map[VkQueryPool]*queryPool
	commandBuffers   map[VkCommandBuffer]*commandBuffer
	images           map[VkImage]*imageLayoutAndData
	buffers          map[VkBuffer]resBindingList
	descriptorSets   map[VkDescriptorSet]*descriptorSet

	// execution info
	executionStates map[VkQueue]*queueExecutionState
	submitInfos     map[api.CmdID] /*ID of VkQueueSubmit*/ *queueSubmitInfo
	submitIDs       map[*VkQueueSubmit]api.CmdID

	// presentation info
	swapchainImageAcquired  map[VkSwapchainKHR][]*label
	swapchainImagePresented map[VkSwapchainKHR][]*label

	// memory
	deviceMemoryRecords *memorySpanRecords
}

// toVkHandle takes the handle value in uint64, check if the build has seen
// the handle before. If not, creates a new vkHandle for the given handle value,
// otherwise, return the seen vkHandle.
func (vb *FootprintBuilder) toVkHandle(handle uint64) *vkHandle {
	if _, ok := vb.handles[handle]; !ok {
		vb.handles[handle] = &vkHandle{handle: handle, b: nil}
	}
	return vb.handles[handle]
}

func (vb *FootprintBuilder) newMemorySpan(mem VkDeviceMemory, offset, size uint64) *memorySpan {
	ms := &memorySpan{memory: mem, sp: interval.U64Span{Start: offset, End: offset + size}}
	if _, ok := vb.deviceMemoryRecords.records[mem]; !ok {
		vb.deviceMemoryRecords.records[mem] = memorySpanList{}
	}
	ms.recordTo = vb.deviceMemoryRecords
	return ms
}

// getImageData records a read operation of the Vulkan image handle, a read
// operation of the image layout, a read operation of the image bindings, then
// returns the underlying data.
func (vb *FootprintBuilder) getImageData(ctx context.Context,
	bh *dependencygraph.Behavior, vkImg VkImage) []dependencygraph.DefUseVariable {
	if bh != nil {
		if !read(ctx, bh, vb.toVkHandle(uint64(vkImg))) {
			return []dependencygraph.DefUseVariable{}
		}
		if !read(ctx, bh, vb.images[vkImg].layout) {
			return []dependencygraph.DefUseVariable{}
		}
	}
	if vb.images[vkImg] == nil {
		return []dependencygraph.DefUseVariable{}
	}
	data := vb.images[vkImg].opaqueData.getBoundData(ctx, bh, 0, vkWholeSize)
	for _, aspecti := range vb.images[vkImg].sparseData {
		for _, layeri := range aspecti {
			for _, leveli := range layeri {
				for _, blocki := range leveli {
					if bh != nil {
						read(ctx, bh, blocki)
					}
					data = append(data, blocki.backingData)
				}
			}
		}
	}
	return data
}

// getImageOpaqueData records a read operation of the Vulkan image handle, a
// read operation of the image layout, a read operation of the overlapping
// bindings, then returns the underlying data. This only works for opaque image
// bindings (non-sparse-resident bindings), and the image must NOT be created
// by swapchains.
func (vb *FootprintBuilder) getImageOpaqueData(ctx context.Context,
	bh *dependencygraph.Behavior, vkImg VkImage, offset, size uint64) []dependencygraph.DefUseVariable {
	read(ctx, bh, vb.toVkHandle(uint64(vkImg)))
	data := vb.images[vkImg].opaqueData.getBoundData(ctx, bh, offset, size)
	return data
}

func (vb *FootprintBuilder) getSparseImageBindData(ctx context.Context,
	cmd api.Cmd, id api.CmdID, s *api.GlobalState, bh *dependencygraph.Behavior,
	vkImg VkImage, bind VkSparseImageMemoryBind) []dependencygraph.DefUseVariable {
	data := []dependencygraph.DefUseVariable{}
	vb.visitBlocksInVkSparseImageMemoryBind(ctx, cmd, id, s, bh, vkImg, bind, func(
		aspects VkImageAspectFlags, layer, level uint32, blockIndex, memoryOffset uint64) {
		if _, ok := vb.images[vkImg].sparseData[aspects]; !ok {
			return
		}
		if _, ok := vb.images[vkImg].sparseData[aspects][layer]; !ok {
			return
		}
		if _, ok := vb.images[vkImg].sparseData[aspects][layer][level]; !ok {
			return
		}
		if _, ok := vb.images[vkImg].sparseData[aspects][layer][level][blockIndex]; !ok {
			return
		}
		data = append(data, vb.images[vkImg].sparseData[aspects][layer][level][blockIndex].backingData)
	})
	return data
}

// getImageLayoutAndData records a read operation of the Vulkan handle, a read
// operation of the image binding, but not the image layout. Then returns the
// image layout label and underlying data.
func (vb *FootprintBuilder) getImageLayoutAndData(ctx context.Context,
	bh *dependencygraph.Behavior, vkImg VkImage) (*label, []dependencygraph.DefUseVariable) {
	read(ctx, bh, vb.toVkHandle(uint64(vkImg)))
	return vb.images[vkImg].layout, vb.getImageData(ctx, bh, vkImg)
}

func (vb *FootprintBuilder) addOpaqueImageMemBinding(ctx context.Context,
	bh *dependencygraph.Behavior, vkImg VkImage, vkMem VkDeviceMemory, resOffset,
	size, memOffset uint64) {
	vb.images[vkImg].opaqueData = addResBinding(ctx, vb.images[vkImg].opaqueData,
		newSpanResBinding(ctx, vb, bh, vkMem, resOffset, size, memOffset))
}

func (vb *FootprintBuilder) addSwapchainImageMemBinding(ctx context.Context,
	bh *dependencygraph.Behavior, vkImg VkImage) {
	vb.images[vkImg].opaqueData = addResBinding(ctx, vb.images[vkImg].opaqueData,
		newNonSpanResBinding(ctx, bh, vkWholeSize))
}

// Traverse through the blocks covered by the given bind.
func (vb *FootprintBuilder) visitBlocksInVkSparseImageMemoryBind(ctx context.Context,
	cmd api.Cmd, id api.CmdID, s *api.GlobalState, bh *dependencygraph.Behavior,
	vkImg VkImage, bind VkSparseImageMemoryBind, cb func(aspects VkImageAspectFlags,
		layer, level uint32, blockIndex, memOffset uint64)) {
	imgObj := GetState(s).Images().Get(vkImg)

	aspect := bind.Subresource().AspectMask()
	layer := bind.Subresource().ArrayLayer()
	level := bind.Subresource().MipLevel()

	gran, found := sparseImageMemoryBindGranularity(ctx, imgObj, bind)
	if found {
		width, _ := subGetMipSize(ctx, cmd, id, nil, s, nil, cmd.Thread(), nil, nil, imgObj.Info().Extent().Width(), level)
		height, _ := subGetMipSize(ctx, cmd, id, nil, s, nil, cmd.Thread(), nil, nil, imgObj.Info().Extent().Height(), level)
		wb, _ := subRoundUpTo(ctx, cmd, id, nil, s, nil, cmd.Thread(), nil, nil, width, gran.Width())
		hb, _ := subRoundUpTo(ctx, cmd, id, nil, s, nil, cmd.Thread(), nil, nil, height, gran.Height())
		xe, _ := subRoundUpTo(ctx, cmd, id, nil, s, nil, cmd.Thread(), nil, nil, bind.Extent().Width(), gran.Width())
		ye, _ := subRoundUpTo(ctx, cmd, id, nil, s, nil, cmd.Thread(), nil, nil, bind.Extent().Height(), gran.Height())
		ze, _ := subRoundUpTo(ctx, cmd, id, nil, s, nil, cmd.Thread(), nil, nil, bind.Extent().Depth(), gran.Depth())
		blockSize := uint64(imgObj.MemoryRequirements().Alignment())
		for zi := uint32(0); zi < ze; zi++ {
			for yi := uint32(0); yi < ye; yi++ {
				for xi := uint32(0); xi < xe; xi++ {
					loc := xi + yi*wb + zi*wb*hb
					memoryOffset := uint64(bind.MemoryOffset()) + uint64(loc)*blockSize
					cb(aspect, layer, level, uint64(loc), memoryOffset)
				}
			}
		}
	}
}

func (vb *FootprintBuilder) addSparseImageMemBinding(ctx context.Context,
	cmd api.Cmd, id api.CmdID,
	s *api.GlobalState, bh *dependencygraph.Behavior, vkImg VkImage,
	bind VkSparseImageMemoryBind) {
	blockSize := GetState(s).Images().Get(vkImg).MemoryRequirements().Alignment()
	vb.visitBlocksInVkSparseImageMemoryBind(ctx, cmd, id, s, bh, vkImg, bind,
		func(aspects VkImageAspectFlags, layer, level uint32, blockIndex, memoryOffset uint64) {
			if _, ok := vb.images[vkImg].sparseData[aspects]; !ok {
				vb.images[vkImg].sparseData[aspects] = map[uint32]map[uint32]map[uint64]*sparseImageMemoryBinding{}
			}
			if _, ok := vb.images[vkImg].sparseData[aspects][layer]; !ok {
				vb.images[vkImg].sparseData[aspects][layer] = map[uint32]map[uint64]*sparseImageMemoryBinding{}
			}
			if _, ok := vb.images[vkImg].sparseData[aspects][layer][level]; !ok {
				vb.images[vkImg].sparseData[aspects][layer][level] = map[uint64]*sparseImageMemoryBinding{}
			}
			vb.images[vkImg].sparseData[aspects][layer][level][blockIndex] = newSparseImageMemoryBinding(
				ctx, vb, bh, bind.Memory(), memoryOffset, uint64(blockSize))
		})
}

func (vb *FootprintBuilder) getBufferData(ctx context.Context,
	bh *dependencygraph.Behavior, vkBuf VkBuffer,
	offset, size uint64) []dependencygraph.DefUseVariable {
	read(ctx, bh, vb.toVkHandle(uint64(vkBuf)))
	for _, bb := range vb.buffers[vkBuf].resBindings() {
		read(ctx, bh, bb)
	}
	return vb.buffers[vkBuf].getBoundData(ctx, bh, offset, size)
}

func (vb *FootprintBuilder) addBufferMemBinding(ctx context.Context,
	bh *dependencygraph.Behavior, vkBuf VkBuffer,
	vkMem VkDeviceMemory, resOffset, size, memOffset uint64) {
	vb.buffers[vkBuf] = addResBinding(ctx, vb.buffers[vkBuf],
		newSpanResBinding(ctx, vb, bh, vkMem, resOffset, size, memOffset))
}

func (vb *FootprintBuilder) newCommand(ctx context.Context,
	bh *dependencygraph.Behavior, vkCb VkCommandBuffer) *commandBufferCommand {
	cbc := &commandBufferCommand{}
	read(ctx, bh, vb.toVkHandle(uint64(vkCb)))
	if _, ok := vb.commandBuffers[vkCb]; ok {
		read(ctx, bh, vb.commandBuffers[vkCb].begin)
		write(ctx, bh, cbc)
		vb.commands[vkCb] = append(vb.commands[vkCb], cbc)
		return cbc
	}
	return nil
}

func newFootprintBuilder() *FootprintBuilder {
	return &FootprintBuilder{
		handles:                 map[uint64]*vkHandle{},
		commands:                map[VkCommandBuffer][]*commandBufferCommand{},
		mappedCoherentMemories:  map[VkDeviceMemory]DeviceMemoryObjectʳ{},
		semaphoreSignals:        map[VkSemaphore]*label{},
		fences:                  map[VkFence]*fence{},
		events:                  map[VkEvent]*event{},
		querypools:              map[VkQueryPool]*queryPool{},
		commandBuffers:          map[VkCommandBuffer]*commandBuffer{},
		images:                  map[VkImage]*imageLayoutAndData{},
		buffers:                 map[VkBuffer]resBindingList{},
		descriptorSets:          map[VkDescriptorSet]*descriptorSet{},
		executionStates:         map[VkQueue]*queueExecutionState{},
		submitInfos:             map[api.CmdID]*queueSubmitInfo{},
		submitIDs:               map[*VkQueueSubmit]api.CmdID{},
		swapchainImageAcquired:  map[VkSwapchainKHR][]*label{},
		swapchainImagePresented: map[VkSwapchainKHR][]*label{},
		deviceMemoryRecords:     &memorySpanRecords{records: map[VkDeviceMemory]memorySpanList{}},
	}
}

func (vb *FootprintBuilder) rollOutExecuted(ctx context.Context,
	ft *dependencygraph.Footprint,
	executedCommands []api.SubCmdIdx) {
	for _, executedFCI := range executedCommands {
		submitID := executedFCI[0]
		submitinfo := vb.submitInfos[api.CmdID(submitID)]
		if !submitinfo.began {
			bh := dependencygraph.NewBehavior(api.SubCmdIdx{submitID})
			for _, sp := range submitinfo.waitSemaphores {
				if read(ctx, bh, vb.toVkHandle(uint64(sp))) {
					modify(ctx, bh, vb.semaphoreSignals[sp])
				}
			}
			// write(ctx, bh, submitinfo.queued)
			ft.AddBehavior(ctx, bh)
			submitinfo.began = true
		}
		submittedCmd := submitinfo.pendingCommands[0]
		if executedFCI.Equals(submittedCmd.id) {
			execInfo := vb.executionStates[submitinfo.queue]
			execInfo.currentSubmitInfo = submitinfo
			execInfo.updateCurrentCommand(ctx, executedFCI)
			submittedCmd.runCommand(ctx, ft, execInfo)
		} else {
			log.E(ctx, "FootprintBuilder: Execution order differs from submission order. "+
				"Index of executed command: %v, Index of submitted command: %v",
				executedFCI, submittedCmd.id)
			return
		}
		// Remove the front submitted command.
		submitinfo.pendingCommands =
			submitinfo.pendingCommands[1:]
		// After the last command of the submit, we need to add a behavior for
		// semaphore and fence signaling.
		if len(submitinfo.pendingCommands) == 0 {
			bh := dependencygraph.NewBehavior(api.SubCmdIdx{
				executedFCI[0]})
			// add writes to the semaphores and fences
			read(ctx, bh, submitinfo.queued)
			write(ctx, bh, submitinfo.done)
			for _, sp := range submitinfo.signalSemaphores {
				if read(ctx, bh, vb.toVkHandle(uint64(sp))) {
					write(ctx, bh, vb.semaphoreSignals[sp])
				}
			}
			if read(ctx, bh, vb.toVkHandle(uint64(submitinfo.signalFence))) {
				write(ctx, bh, vb.fences[submitinfo.signalFence].signal)
			}
			ft.AddBehavior(ctx, bh)
		}
	}
}

func (vb *FootprintBuilder) recordReadsWritesModifies(
	ctx context.Context, ft *dependencygraph.Footprint, bh *dependencygraph.Behavior,
	vkCb VkCommandBuffer, reads []dependencygraph.DefUseVariable,
	writes []dependencygraph.DefUseVariable, modifies []dependencygraph.DefUseVariable) {
	cbc := vb.newCommand(ctx, bh, vkCb)
	cbc.behave = func(sc submittedCommand, execInfo *queueExecutionState) {
		cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
		read(ctx, cbh, reads...)
		write(ctx, cbh, writes...)
		modify(ctx, cbh, modifies...)
		ft.AddBehavior(ctx, cbh)
	}
}

func (vb *FootprintBuilder) recordModifingDynamicStates(
	ctx context.Context, ft *dependencygraph.Footprint, bh *dependencygraph.Behavior,
	vkCb VkCommandBuffer) {
	cbc := vb.newCommand(ctx, bh, vkCb)
	cbc.behave = func(sc submittedCommand, execInfo *queueExecutionState) {
		cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
		modify(ctx, cbh, execInfo.currentCmdBufState.dynamicState)
		ft.AddBehavior(ctx, cbh)
	}
}

func (vb *FootprintBuilder) useBoundDescriptorSets(ctx context.Context,
	bh *dependencygraph.Behavior,
	cmdBufState *commandBufferExecutionState) []dependencygraph.DefUseVariable {
	modified := []dependencygraph.DefUseVariable{}
	for _, bds := range cmdBufState.descriptorSets {
		read(ctx, bh, bds)
		ds := bds.descriptorSet
		modified = append(modified, ds.useDescriptors(ctx, vb, bh, bds.dynamicOffsets)...)
	}
	return modified
}

func (vb *FootprintBuilder) draw(ctx context.Context,
	bh *dependencygraph.Behavior, execInfo *queueExecutionState) {
	read(ctx, bh, execInfo.subpass)
	read(ctx, bh, execInfo.currentCmdBufState.pipeline)
	read(ctx, bh, execInfo.currentCmdBufState.dynamicState)
	subpassI := execInfo.subpass.val
	for _, b := range execInfo.currentCmdBufState.vertexBufferResBindings {
		read(ctx, bh, b.getBoundData(ctx, bh, 0, vkWholeSize)...)
	}
	modifiedDs := vb.useBoundDescriptorSets(ctx, bh, execInfo.currentCmdBufState)
	execInfo.subpasses[execInfo.subpass.val].modifiedDescriptorData = append(
		execInfo.subpasses[execInfo.subpass.val].modifiedDescriptorData,
		modifiedDs...)
	if execInfo.currentCmdBufState.indexBufferResBindings != nil {
		read(ctx, bh, execInfo.currentCmdBufState.indexBufferResBindings.getBoundData(
			ctx, bh, 0, vkWholeSize)...)
	}
	for _, input := range execInfo.subpasses[subpassI].inputAttachments {
		read(ctx, bh, input.data...)
	}
	for _, color := range execInfo.subpasses[subpassI].colorAttachments {
		modify(ctx, bh, color.data...)
	}
	if execInfo.subpasses[subpassI].depthStencilAttachment != nil {
		dsAtt := execInfo.subpasses[subpassI].depthStencilAttachment
		modify(ctx, bh, dsAtt.data...)
	}
}

func (vb *FootprintBuilder) keepSubmittedCommandAlive(ctx context.Context,
	ft *dependencygraph.Footprint, bh *dependencygraph.Behavior,
	vkCb VkCommandBuffer) {
	cbc := vb.newCommand(ctx, bh, vkCb)
	cbc.behave = func(sc submittedCommand, execInfo *queueExecutionState) {
		cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
		cbh.Alive = true
		ft.AddBehavior(ctx, cbh)
	}
}

func (t VkIndexType) size() int {
	switch t {
	case VkIndexType_VK_INDEX_TYPE_UINT16:
		return 2
	case VkIndexType_VK_INDEX_TYPE_UINT32:
		return 4
	default:
		return 0
	}
	return 0
}

func (vb *FootprintBuilder) readBoundIndexBuffer(ctx context.Context,
	bh *dependencygraph.Behavior, execInfo *queueExecutionState, cmd api.Cmd) {
	indexSize := uint64(execInfo.currentCmdBufState.indexType.size())
	if indexSize == uint64(0) {
		log.E(ctx, "FootprintBuilder: Invalid size of the indices of bound index buffer. IndexType: %v",
			execInfo.currentCmdBufState.indexType)
	}
	offset := uint64(0)
	size := vkWholeSize
	switch cmd := cmd.(type) {
	case *VkCmdDrawIndexed:
		size = uint64(cmd.IndexCount()) * indexSize
		offset += uint64(cmd.FirstIndex()) * indexSize
	case *VkCmdDrawIndexedIndirect:
	}
	dataToRead := execInfo.currentCmdBufState.indexBufferResBindings.getBoundData(
		ctx, bh, offset, size)
	read(ctx, bh, dataToRead...)
}

func (vb *FootprintBuilder) recordBarriers(ctx context.Context,
	s *api.GlobalState, ft *dependencygraph.Footprint, cmd api.Cmd,
	bh *dependencygraph.Behavior, vkCb VkCommandBuffer, memoryBarrierCount uint32,
	bufferBarrierCount uint32, pBufferBarriers VkBufferMemoryBarrierᶜᵖ,
	imageBarrierCount uint32, pImageBarriers VkImageMemoryBarrierᶜᵖ,
	attachedReads []dependencygraph.DefUseVariable) {
	l := s.MemoryLayout
	touchedData := []dependencygraph.DefUseVariable{}
	if memoryBarrierCount > 0 {
		// touch all buffer and image backing data
		for i := range vb.images {
			touchedData = append(touchedData, vb.getImageData(ctx, bh, i)...)
		}
		for b := range vb.buffers {
			touchedData = append(touchedData, vb.getBufferData(ctx, bh, b, 0, vkWholeSize)...)
		}
	} else {
		for _, barrier := range pBufferBarriers.Slice(0,
			uint64(bufferBarrierCount), l).MustRead(ctx, cmd, s, nil) {
			touchedData = append(touchedData, vb.getBufferData(ctx, bh, barrier.Buffer(),
				uint64(barrier.Offset()), uint64(barrier.Size()))...)
		}
		for _, barrier := range pImageBarriers.Slice(0,
			uint64(imageBarrierCount), l).MustRead(ctx, cmd, s, nil) {
			imgLayout, imgData := vb.getImageLayoutAndData(ctx, bh, barrier.Image())
			touchedData = append(touchedData, imgLayout)
			touchedData = append(touchedData, imgData...)
		}
	}
	cbc := vb.newCommand(ctx, bh, vkCb)
	cbc.behave = func(sc submittedCommand,
		execInfo *queueExecutionState) {
		for _, d := range touchedData {
			cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
			read(ctx, cbh, attachedReads...)
			modify(ctx, cbh, d)
			ft.AddBehavior(ctx, cbh)
		}
		cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
		read(ctx, cbh, attachedReads...)
		ft.AddBehavior(ctx, cbh)
	}
}

// BuildFootprint incrementally builds the given Footprint with the given
// command specified with api.CmdID and api.Cmd.
func (vb *FootprintBuilder) BuildFootprint(ctx context.Context,
	s *api.GlobalState, ft *dependencygraph.Footprint, id api.CmdID, cmd api.Cmd) {

	l := s.MemoryLayout

	// Records the mapping from queue submit to command ID, so the
	// HandleSubcommand callback can use it.
	if qs, isSubmit := cmd.(*VkQueueSubmit); isSubmit {
		vb.submitIDs[qs] = id
	}
	// Register callback function to record only the truly executed
	// commandbuffer commands.
	executedCommands := []api.SubCmdIdx{}
	GetState(s).PostSubcommand = func(a interface{}) {
		queueSubmit, isQs := (GetState(s).CurrentSubmission).(*VkQueueSubmit)
		if !isQs {
			log.E(ctx, "FootprintBuilder: CurrentSubmission command in State is not a VkQueueSubmit")
		}
		fci := api.SubCmdIdx{uint64(vb.submitIDs[queueSubmit])}
		fci = append(fci, GetState(s).SubCmdIdx...)
		executedCommands = append(executedCommands, fci)
	}

	// Register callback function to track sparse bindings
	sparseBindingInfo := []QueuedSparseBinds{}
	GetState(s).postBindSparse = func(binds QueuedSparseBindsʳ) {
		sparseBindingInfo = append(sparseBindingInfo, binds.Get())
	}

	// Mutate
	if err := cmd.Mutate(ctx, id, s, nil, nil); err != nil {
		// Continue the footprint building without emitting errors here. It is the
		// following mutate() calls' responsibility to catch the error.
		return
	}

	bh := dependencygraph.NewBehavior(api.SubCmdIdx{uint64(id)})

	// The main switch
	switch cmd := cmd.(type) {
	// device memory
	case *VkAllocateMemory:
		vkMem := cmd.PMemory().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(vkMem)))
	case *VkFreeMemory:
		vkMem := cmd.Memory()
		read(ctx, bh, vb.toVkHandle(uint64(vkMem)))
		bh.Alive = true
	case *VkMapMemory:
		modify(ctx, bh, vb.toVkHandle(uint64(cmd.Memory())))
		memObj := GetState(s).DeviceMemories().Get(cmd.Memory())
		isCoherent, _ := subIsMemoryCoherent(ctx, cmd, id, nil, s, GetState(s),
			cmd.Thread(), nil, nil, memObj)
		if isCoherent {
			vb.mappedCoherentMemories[cmd.Memory()] = memObj
		}
		bh.Alive = true
	case *VkUnmapMemory:
		modify(ctx, bh, vb.toVkHandle(uint64(cmd.Memory())))
		vb.writeCoherentMemoryData(ctx, cmd, bh)
		if _, mappedCoherent := vb.mappedCoherentMemories[cmd.Memory()]; mappedCoherent {
			delete(vb.mappedCoherentMemories, cmd.Memory())
		}
		bh.Alive = true
	case *VkFlushMappedMemoryRanges:
		coherentMemDone := false
		count := uint64(cmd.MemoryRangeCount())
		for _, rng := range cmd.PMemoryRanges().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			read(ctx, bh, vb.toVkHandle(uint64(rng.Memory())))
			mem := GetState(s).DeviceMemories().Get(rng.Memory())
			if mem.IsNil() {
				continue
			}
			isCoherent, _ := subIsMemoryCoherent(ctx, cmd, id, nil, s, GetState(s), cmd.Thread(), nil, nil, mem)
			if isCoherent {
				if !coherentMemDone {
					vb.writeCoherentMemoryData(ctx, cmd, bh)
					coherentMemDone = true
				}
				continue
			}
			offset := uint64(rng.Offset())
			size := uint64(rng.Size())
			ms := vb.newMemorySpan(rng.Memory(), offset, size)
			// ms := &memorySpan{
			// 	sp:     interval.U64Span{Start: offset, End: offset + size},
			// 	memory: rng.Memory(),
			// }
			write(ctx, bh, ms)
		}
	case *VkInvalidateMappedMemoryRanges:
		count := uint64(cmd.MemoryRangeCount())
		for _, rng := range cmd.PMemoryRanges().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			read(ctx, bh, vb.toVkHandle(uint64(rng.Memory())))
			offset := uint64(rng.Offset())
			size := uint64(rng.Size())
			ms := vb.newMemorySpan(rng.Memory(), offset, size)
			// ms := &memorySpan{
			// 	sp:     interval.U64Span{Start: offset, End: offset + size},
			// 	memory: rng.Memory(),
			// }
			read(ctx, bh, ms)
		}

	// image
	case *VkCreateImage:
		vkImg := cmd.PImage().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(vkImg)))
		vb.images[vkImg] = newImageLayoutAndData(ctx, bh)
	case *VkDestroyImage:
		vkImg := cmd.Image()
		if read(ctx, bh, vb.toVkHandle(uint64(vkImg))) {
			delete(vb.images, vkImg)
		}
		bh.Alive = true
	case *VkGetImageMemoryRequirements:
		// TODO: Once the memory requirements are moved out from the image object,
		// drop the 'modify' on the image handle, replace it with another proper
		// representation of the cached data.
		modify(ctx, bh, vb.toVkHandle(uint64(cmd.Image())))
	case *VkGetImageSparseMemoryRequirements:
		// TODO: Once the memory requirements are moved out from the image object,
		// drop the 'modify' on the image handle, replace it with another proper
		// representation of the cached data.
		modify(ctx, bh, vb.toVkHandle(uint64(cmd.Image())))

	case *ReplayAllocateImageMemory:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Image())))
		vkMem := cmd.PMemory().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(vkMem)))
	case *VkBindImageMemory:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Image())))
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Memory())))
		offset := uint64(cmd.MemoryOffset())
		inferredSize, err := subInferImageSize(ctx, cmd, id, nil, s, nil, cmd.Thread(),
			nil, nil, GetState(s).Images().Get(cmd.Image()))
		if err != nil {
			log.E(ctx, "FootprintBuilder: Cannot get inferred size of image: %v", cmd.Image())
			log.E(ctx, "FootprintBuilder: Command %v %v: %v", id, cmd, err)
			bh.Aborted = true
		}
		size := uint64(inferredSize)
		vb.addOpaqueImageMemBinding(ctx, bh, cmd.Image(), cmd.Memory(), 0, size, offset)

	case *VkCreateImageView:
		write(ctx, bh, vb.toVkHandle(uint64(cmd.PView().MustRead(ctx, cmd, s, nil))))
		img := cmd.PCreateInfo().MustRead(ctx, cmd, s, nil).Image()
		read(ctx, bh, vb.getImageData(ctx, bh, img)...)
	case *VkDestroyImageView:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.ImageView())))
		bh.Alive = true

	// buffer
	case *VkCreateBuffer:
		vkBuf := cmd.PBuffer().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(vkBuf)))
	case *VkDestroyBuffer:
		vkBuf := cmd.Buffer()
		if read(ctx, bh, vb.toVkHandle(uint64(vkBuf))) {
			delete(vb.buffers, vkBuf)
		}
		bh.Alive = true
	case *VkGetBufferMemoryRequirements:
		// TODO: Once the memory requirements are moved out from the buffer object,
		// drop the 'modify' on the buffer handle, replace it with another proper
		// representation of the cached data.
		modify(ctx, bh, vb.toVkHandle(uint64(cmd.Buffer())))

	case *VkBindBufferMemory:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Buffer())))
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Memory())))
		offset := uint64(cmd.MemoryOffset())
		size := uint64(GetState(s).Buffers().Get(cmd.Buffer()).Info().Size())
		vb.addBufferMemBinding(ctx, bh, cmd.Buffer(), cmd.Memory(), 0, size, offset)
	case *VkCreateBufferView:
		write(ctx, bh, vb.toVkHandle(uint64(cmd.PView().MustRead(ctx, cmd, s, nil))))
		info := cmd.PCreateInfo().MustRead(ctx, cmd, s, nil)
		buf := info.Buffer()
		offset := uint64(info.Offset())
		size := uint64(info.Range())
		read(ctx, bh, vb.getBufferData(ctx, bh, buf, offset, size)...)
	case *VkDestroyBufferView:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.BufferView())))
		bh.Alive = true

	// swapchain
	case *VkCreateSwapchainKHR:
		vkSw := cmd.PSwapchain().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(vkSw)))

	case *VkCreateSharedSwapchainsKHR:
		count := uint64(cmd.SwapchainCount())
		for _, vkSw := range cmd.PSwapchains().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			write(ctx, bh, vb.toVkHandle(uint64(vkSw)))
		}

	case *VkGetSwapchainImagesKHR:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Swapchain())))
		if cmd.PSwapchainImages() == 0 {
			modify(ctx, bh, vb.toVkHandle(uint64(cmd.Swapchain())))
		} else {
			count := uint64(cmd.PSwapchainImageCount().MustRead(ctx, cmd, s, nil))
			for _, vkImg := range cmd.PSwapchainImages().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
				write(ctx, bh, vb.toVkHandle(uint64(vkImg)))
				vb.images[vkImg] = newImageLayoutAndData(ctx, bh)
				vb.addSwapchainImageMemBinding(ctx, bh, vkImg)
				vb.swapchainImageAcquired[cmd.Swapchain()] = append(
					vb.swapchainImageAcquired[cmd.Swapchain()], newLabel())
				vb.swapchainImagePresented[cmd.Swapchain()] = append(
					vb.swapchainImagePresented[cmd.Swapchain()], newLabel())
			}
		}
	case *VkDestroySwapchainKHR:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Swapchain())))
		delete(vb.swapchainImageAcquired, cmd.Swapchain())
		delete(vb.swapchainImagePresented, cmd.Swapchain())
		bh.Alive = true

	// presentation engine
	case *VkAcquireNextImageKHR:
		if read(ctx, bh, vb.toVkHandle(uint64(cmd.Semaphore()))) {
			write(ctx, bh, vb.semaphoreSignals[cmd.Semaphore()])
		}
		if read(ctx, bh, vb.toVkHandle(uint64(cmd.Fence()))) {
			write(ctx, bh, vb.fences[cmd.Fence()].signal)
		}
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Swapchain())))
		// The value of this imgId should have been written by the driver.
		imgID := cmd.PImageIndex().MustRead(ctx, cmd, s, nil)
		vkImg := GetState(s).Swapchains().Get(cmd.Swapchain()).SwapchainImages().Get(imgID).VulkanHandle()
		if read(ctx, bh, vb.toVkHandle(uint64(vkImg))) {
			imgLayout, imgData := vb.getImageLayoutAndData(ctx, bh, vkImg)
			write(ctx, bh, imgLayout)
			write(ctx, bh, imgData...)
		}
		write(ctx, bh, vb.swapchainImageAcquired[cmd.Swapchain()][imgID])
		read(ctx, bh, vb.swapchainImagePresented[cmd.Swapchain()][imgID])

	case *VkQueuePresentKHR:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Queue())))
		info := cmd.PPresentInfo().MustRead(ctx, cmd, s, nil)
		spCount := uint64(info.WaitSemaphoreCount())
		for _, vkSp := range info.PWaitSemaphores().Slice(0, spCount, l).MustRead(ctx, cmd, s, nil) {
			if read(ctx, bh, vb.toVkHandle(uint64(vkSp))) {
				read(ctx, bh, vb.semaphoreSignals[vkSp])
			}
		}
		swCount := uint64(info.SwapchainCount())
		imgIds := info.PImageIndices().Slice(0, swCount, l)
		for swi, vkSw := range info.PSwapchains().Slice(0, swCount, l).MustRead(ctx, cmd, s, nil) {
			read(ctx, bh, vb.toVkHandle(uint64(vkSw)))
			imgID := imgIds.Index(uint64(swi)).MustRead(ctx, cmd, s, nil)[0]
			vkImg := GetState(s).Swapchains().Get(vkSw).SwapchainImages().Get(imgID).VulkanHandle()
			imgLayout, imgData := vb.getImageLayoutAndData(ctx, bh, vkImg)
			read(ctx, bh, imgLayout)
			read(ctx, bh, imgData...)

			// For each image to be presented, one extra behavior is requied to
			// track the acquire-present pair of the image state in the presentation
			// engine. And this extra behavior must be kept alive to prevent the
			// presentation engine from hang.
			extraBh := dependencygraph.NewBehavior(api.SubCmdIdx{uint64(id)})
			for _, vkSp := range info.PWaitSemaphores().Slice(0, spCount, l).MustRead(ctx, cmd, s, nil) {
				read(ctx, extraBh, vb.toVkHandle(uint64(cmd.Queue())))
				if read(ctx, extraBh, vb.toVkHandle(uint64(vkSp))) {
					read(ctx, extraBh, vb.semaphoreSignals[vkSp])
				}
			}
			read(ctx, extraBh, vb.swapchainImageAcquired[vkSw][imgID])
			write(ctx, extraBh, vb.swapchainImagePresented[vkSw][imgID])
			extraBh.Alive = true
			ft.AddBehavior(ctx, extraBh)
		}

	// sampler
	case *VkCreateSampler:
		write(ctx, bh, vb.toVkHandle(uint64(cmd.PSampler().MustRead(ctx, cmd, s, nil))))
	case *VkDestroySampler:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Sampler())))
		bh.Alive = true

	// query pool
	case *VkCreateQueryPool:
		vkQp := cmd.PQueryPool().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(vkQp)))
		count := uint64(cmd.PCreateInfo().MustRead(ctx, cmd, s, nil).QueryCount())
		vb.querypools[vkQp] = &queryPool{
			queries: make([]*query, 0, count),
		}
		for i := uint64(0); i < count; i++ {
			vb.querypools[vkQp].queries = append(vb.querypools[vkQp].queries, newQuery())
		}
	case *VkDestroyQueryPool:
		if read(ctx, bh, vb.toVkHandle(uint64(cmd.QueryPool()))) {
			delete(vb.querypools, cmd.QueryPool())
		}
		bh.Alive = true
	case *VkGetQueryPoolResults:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.QueryPool())))
		count := uint64(cmd.QueryCount())
		first := uint64(cmd.FirstQuery())
		for i := uint64(0); i < count; i++ {
			read(ctx, bh, vb.querypools[cmd.QueryPool()].queries[i+first].result)
		}

	// descriptor set
	case *VkCreateDescriptorSetLayout:
		write(ctx, bh, vb.toVkHandle(uint64(cmd.PSetLayout().MustRead(ctx, cmd, s, nil))))
		info := cmd.PCreateInfo().MustRead(ctx, cmd, s, nil)
		bindings := info.PBindings().Slice(0, uint64(info.BindingCount()), l).MustRead(ctx, cmd, s, nil)
		for _, b := range bindings {
			if b.PImmutableSamplers() != memory.Nullptr {
				samplers := b.PImmutableSamplers().Slice(0, uint64(b.DescriptorCount()), l).MustRead(ctx, cmd, s, nil)
				for _, sam := range samplers {
					read(ctx, bh, vb.toVkHandle(uint64(sam)))
				}
			}
		}
	case *VkDestroyDescriptorSetLayout:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.DescriptorSetLayout())))
		bh.Alive = true
	case *VkAllocateDescriptorSets:
		info := cmd.PAllocateInfo().MustRead(ctx, cmd, s, nil)
		setCount := uint64(info.DescriptorSetCount())
		vkLayouts := info.PSetLayouts().Slice(0, setCount, l)
		for i, vkSet := range cmd.PDescriptorSets().Slice(0, setCount, l).MustRead(ctx, cmd, s, nil) {
			vkLayout := vkLayouts.Index(uint64(i)).MustRead(ctx, cmd, s, nil)[0]
			read(ctx, bh, vb.toVkHandle(uint64(vkLayout)))
			layoutObj := GetState(s).DescriptorSetLayouts().Get(vkLayout)
			write(ctx, bh, vb.toVkHandle(uint64(vkSet)))
			vb.descriptorSets[vkSet] = newDescriptorSet()
			for bi, bindingInfo := range layoutObj.Bindings().All() {
				for di := uint32(0); di < bindingInfo.Count(); di++ {
					vb.descriptorSets[vkSet].reserveDescriptor(uint64(bi), uint64(di))
				}
			}
		}
	case *VkUpdateDescriptorSets:
		writeCount := cmd.DescriptorWriteCount()
		if writeCount > 0 {
			for _, write := range cmd.PDescriptorWrites().Slice(0, uint64(writeCount),
				l).MustRead(ctx, cmd, s, nil) {
				read(ctx, bh, vb.toVkHandle(uint64(write.DstSet())))
				ds := vb.descriptorSets[write.DstSet()]
				ds.writeDescriptors(ctx, cmd, s, vb, bh, write)
			}
		}
		copyCount := cmd.DescriptorCopyCount()
		if copyCount > 0 {
			for _, copy := range cmd.PDescriptorCopies().Slice(0, uint64(copyCount),
				l).MustRead(ctx, cmd, s, nil) {
				read(ctx, bh, vb.toVkHandle(uint64(copy.SrcSet())))
				read(ctx, bh, vb.toVkHandle(uint64(copy.DstSet())))
				vb.descriptorSets[copy.DstSet()].copyDescriptors(ctx, cmd, s, bh,
					vb.descriptorSets[copy.SrcSet()], copy)
			}
		}

	case *VkFreeDescriptorSets:
		count := uint64(cmd.DescriptorSetCount())
		for _, vkSet := range cmd.PDescriptorSets().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			read(ctx, bh, vb.toVkHandle(uint64(vkSet)))
			delete(vb.descriptorSets, vkSet)
		}
		bh.Alive = true

	// pipelines
	case *VkCreatePipelineLayout:
		info := cmd.PCreateInfo().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(cmd.PPipelineLayout().MustRead(ctx, cmd, s, nil))))
		setCount := uint64(info.SetLayoutCount())
		for _, setLayout := range info.PSetLayouts().Slice(0, setCount, l).MustRead(ctx, cmd, s, nil) {
			read(ctx, bh, vb.toVkHandle(uint64(setLayout)))
		}
	case *VkDestroyPipelineLayout:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.PipelineLayout())))
		bh.Alive = true
	case *VkCreateGraphicsPipelines:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.PipelineCache())))
		infoCount := uint64(cmd.CreateInfoCount())
		for _, info := range cmd.PCreateInfos().Slice(0, infoCount, l).MustRead(ctx, cmd, s, nil) {
			stageCount := uint64(info.StageCount())
			for _, stage := range info.PStages().Slice(0, stageCount, l).MustRead(ctx, cmd, s, nil) {
				module := stage.Module()
				read(ctx, bh, vb.toVkHandle(uint64(module)))
			}
			read(ctx, bh, vb.toVkHandle(uint64(info.Layout())))
			read(ctx, bh, vb.toVkHandle(uint64(info.RenderPass())))
		}
		for _, vkPl := range cmd.PPipelines().Slice(0, infoCount, l).MustRead(ctx, cmd, s, nil) {
			write(ctx, bh, vb.toVkHandle(uint64(vkPl)))
		}
	case *VkCreateComputePipelines:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.PipelineCache())))
		infoCount := uint64(cmd.CreateInfoCount())
		for _, info := range cmd.PCreateInfos().Slice(0, infoCount, l).MustRead(ctx, cmd, s, nil) {
			stage := info.Stage()
			module := stage.Module()
			read(ctx, bh, vb.toVkHandle(uint64(module)))
			read(ctx, bh, vb.toVkHandle(uint64(info.Layout())))
		}
		for _, vkPl := range cmd.PPipelines().Slice(0, infoCount, l).MustRead(ctx, cmd, s, nil) {
			write(ctx, bh, vb.toVkHandle(uint64(vkPl)))
		}
	case *VkDestroyPipeline:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Pipeline())))
		bh.Alive = true

	case *VkCreatePipelineCache:
		write(ctx, bh, vb.toVkHandle(uint64(cmd.PPipelineCache().MustRead(ctx, cmd, s, nil))))
	case *VkDestroyPipelineCache:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.PipelineCache())))
		bh.Alive = true
	case *VkGetPipelineCacheData:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.PipelineCache())))
	case *VkMergePipelineCaches:
		modify(ctx, bh, vb.toVkHandle(uint64(cmd.DstCache())))
		srcCount := uint64(cmd.SrcCacheCount())
		for _, src := range cmd.PSrcCaches().Slice(0, srcCount, l).MustRead(ctx, cmd, s, nil) {
			read(ctx, bh, vb.toVkHandle(uint64(src)))
		}

	// Shader module
	case *VkCreateShaderModule:
		write(ctx, bh, vb.toVkHandle(uint64(cmd.PShaderModule().MustRead(ctx, cmd, s, nil))))
	case *VkDestroyShaderModule:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.ShaderModule())))
		bh.Alive = true

	// create/destroy renderpass
	case *VkCreateRenderPass:
		write(ctx, bh, vb.toVkHandle(uint64(cmd.PRenderPass().MustRead(ctx, cmd, s, nil))))
	case *VkDestroyRenderPass:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.RenderPass())))
		bh.Alive = true

	// create/destroy framebuffer
	case *VkCreateFramebuffer:
		info := cmd.PCreateInfo().MustRead(ctx, cmd, s, nil)
		read(ctx, bh, vb.toVkHandle(uint64(info.RenderPass())))
		attCount := uint64(info.AttachmentCount())
		for _, att := range info.PAttachments().Slice(0, attCount, l).MustRead(ctx, cmd, s, nil) {
			read(ctx, bh, vb.toVkHandle(uint64(att)))
		}
		write(ctx, bh, vb.toVkHandle(uint64(cmd.PFramebuffer().MustRead(ctx, cmd, s, nil))))
	case *VkDestroyFramebuffer:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Framebuffer())))
		bh.Alive = true

	// debug marker name and tag setting commands. Always kept alive.
	case *VkDebugMarkerSetObjectTagEXT:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.PTagInfo().MustRead(ctx, cmd, s, nil).Object())))
		bh.Alive = true
	case *VkDebugMarkerSetObjectNameEXT:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.PNameInfo().MustRead(ctx, cmd, s, nil).Object())))
		bh.Alive = true

	// commandbuffer
	case *VkAllocateCommandBuffers:
		count := uint64(cmd.PAllocateInfo().MustRead(ctx, cmd, s, nil).CommandBufferCount())
		for _, vkCb := range cmd.PCommandBuffers().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			write(ctx, bh, vb.toVkHandle(uint64(vkCb)))
			vb.commandBuffers[vkCb] = &commandBuffer{begin: newLabel(),
				end: newLabel(), renderPassBegin: newLabel()}
		}

	case *VkResetCommandBuffer:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.CommandBuffer())))
		if _, ok := vb.commandBuffers[cmd.CommandBuffer()]; ok {
			write(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].begin)
			write(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].end)
			vb.commands[cmd.CommandBuffer()] = []*commandBufferCommand{}
		}

	case *VkFreeCommandBuffers:
		count := uint64(cmd.CommandBufferCount())
		for _, vkCb := range cmd.PCommandBuffers().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			if _, ok := vb.commandBuffers[vkCb]; ok {
				if read(ctx, bh, vb.toVkHandle(uint64(vkCb))) {
					write(ctx, bh, vb.commandBuffers[vkCb].begin)
					write(ctx, bh, vb.commandBuffers[vkCb].end)
					delete(vb.commandBuffers, vkCb)
					delete(vb.commands, vkCb)
				}
			}
		}
		bh.Alive = true

	case *VkBeginCommandBuffer:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.CommandBuffer())))
		if _, ok := vb.commandBuffers[cmd.CommandBuffer()]; ok {
			write(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].begin)
			vb.commands[cmd.CommandBuffer()] = []*commandBufferCommand{}
		}
	case *VkEndCommandBuffer:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.CommandBuffer())))
		if _, ok := vb.commandBuffers[cmd.CommandBuffer()]; ok {
			read(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].begin)
			write(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].end)
		}

	// copy, blit, resolve, clear, fill, update image and buffer
	case *VkCmdCopyImage:
		dst := vb.getImageData(ctx, bh, cmd.DstImage())
		src := vb.getImageData(ctx, bh, cmd.SrcImage())
		overwritten := false
		count := uint64(cmd.RegionCount())
		// TODO: check dst image coverage correctly
		for _, region := range cmd.PRegions().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			overwritten = overwritten || subresourceLayersFullyCoverImage(
				GetState(s).Images().Get(cmd.DstImage()),
				region.DstSubresource(), region.DstOffset(), region.Extent())
		}
		if overwritten {
			vb.recordReadsWritesModifies(
				ctx, ft, bh, cmd.CommandBuffer(), src, dst, emptyDefUseVars)
		} else {
			vb.recordReadsWritesModifies(
				ctx, ft, bh, cmd.CommandBuffer(), src, emptyDefUseVars, dst)
		}

	case *VkCmdCopyBuffer:
		src := []dependencygraph.DefUseVariable{}
		dst := []dependencygraph.DefUseVariable{}
		count := uint64(cmd.RegionCount())
		for _, region := range cmd.PRegions().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			src = append(src, vb.getBufferData(ctx, bh, cmd.SrcBuffer(),
				uint64(region.SrcOffset()), uint64(region.Size()))...)
			dst = append(dst, vb.getBufferData(ctx, bh, cmd.DstBuffer(),
				uint64(region.DstOffset()), uint64(region.Size()))...)
		}
		vb.recordReadsWritesModifies(
			ctx, ft, bh, cmd.CommandBuffer(), src, dst, emptyDefUseVars)

	case *VkCmdCopyImageToBuffer:
		// TODO: calculate the ranges for the overwritten data
		dst := vb.getBufferData(ctx, bh, cmd.DstBuffer(), 0, vkWholeSize)
		src := vb.getImageData(ctx, bh, cmd.SrcImage())
		vb.recordReadsWritesModifies(
			ctx, ft, bh, cmd.CommandBuffer(), src, emptyDefUseVars, dst)

	case *VkCmdCopyBufferToImage:
		// TODO: calculate the ranges for the source data
		src := vb.getBufferData(ctx, bh, cmd.SrcBuffer(), 0, vkWholeSize)
		dst := vb.getImageData(ctx, bh, cmd.DstImage())
		overwritten := false
		count := uint64(cmd.RegionCount())
		for _, region := range cmd.PRegions().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			overwritten = overwritten || subresourceLayersFullyCoverImage(
				GetState(s).Images().Get(cmd.DstImage()),
				region.ImageSubresource(), region.ImageOffset(), region.ImageExtent())
		}
		if overwritten {
			vb.recordReadsWritesModifies(
				ctx, ft, bh, cmd.CommandBuffer(), src, dst, emptyDefUseVars)
		} else {
			vb.recordReadsWritesModifies(
				ctx, ft, bh, cmd.CommandBuffer(), src, emptyDefUseVars, dst)
		}

	case *VkCmdBlitImage:
		src := vb.getImageData(ctx, bh, cmd.SrcImage())
		dst := vb.getImageData(ctx, bh, cmd.DstImage())
		overwritten := false
		count := uint64(cmd.RegionCount())
		for _, region := range cmd.PRegions().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			overwritten = overwritten || blitFullyCoverImage(
				GetState(s).Images().Get(cmd.DstImage()),
				region.DstSubresource(),
				region.DstOffsets().Get(0), region.DstOffsets().Get(1))
		}
		if overwritten {
			vb.recordReadsWritesModifies(
				ctx, ft, bh, cmd.CommandBuffer(), src, dst, emptyDefUseVars)
		} else {
			vb.recordReadsWritesModifies(
				ctx, ft, bh, cmd.CommandBuffer(), src, emptyDefUseVars, dst)
		}

	case *VkCmdResolveImage:
		src := vb.getImageData(ctx, bh, cmd.SrcImage())
		dst := vb.getImageData(ctx, bh, cmd.DstImage())
		overwritten := false
		count := uint64(cmd.RegionCount())
		for _, region := range cmd.PRegions().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			overwritten = overwritten || subresourceLayersFullyCoverImage(
				GetState(s).Images().Get(cmd.DstImage()),
				region.DstSubresource(), region.DstOffset(), region.Extent())
		}
		if overwritten {
			vb.recordReadsWritesModifies(
				ctx, ft, bh, cmd.CommandBuffer(), src, dst, emptyDefUseVars)
		} else {
			vb.recordReadsWritesModifies(
				ctx, ft, bh, cmd.CommandBuffer(), src, emptyDefUseVars, dst)
		}

	case *VkCmdFillBuffer:
		dst := vb.getBufferData(ctx, bh, cmd.DstBuffer(), uint64(cmd.DstOffset()), uint64(cmd.Size()))
		vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(),
			emptyDefUseVars, dst, emptyDefUseVars)

	case *VkCmdUpdateBuffer:
		dst := vb.getBufferData(ctx, bh, cmd.DstBuffer(), uint64(cmd.DstOffset()), uint64(cmd.DataSize()))
		vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(),
			emptyDefUseVars, dst, emptyDefUseVars)

	case *VkCmdClearColorImage:
		dst := vb.getImageData(ctx, bh, cmd.Image())
		count := uint64(cmd.RangeCount())
		overwritten := false
		for _, rng := range cmd.PRanges().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			if subresourceRangeFullyCoverImage(GetState(s).Images().Get(cmd.Image()), rng) {
				overwritten = true
			}
		}
		if overwritten {
			vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(),
				emptyDefUseVars, dst, emptyDefUseVars)
		} else {
			vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(),
				emptyDefUseVars, emptyDefUseVars, dst)
		}

	case *VkCmdClearDepthStencilImage:
		dst := vb.getImageData(ctx, bh, cmd.Image())
		count := uint64(cmd.RangeCount())
		overwritten := false
		for _, rng := range cmd.PRanges().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			if subresourceRangeFullyCoverImage(GetState(s).Images().Get(cmd.Image()), rng) {
				overwritten = true
			}
		}
		if overwritten {
			vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(),
				emptyDefUseVars, dst, emptyDefUseVars)
		} else {
			vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(),
				emptyDefUseVars, emptyDefUseVars, dst)
		}

	// renderpass and subpass
	case *VkCmdBeginRenderPass:
		vkRp := cmd.PRenderPassBegin().MustRead(ctx, cmd, s, nil).RenderPass()
		read(ctx, bh, vb.toVkHandle(uint64(vkRp)))
		vkFb := cmd.PRenderPassBegin().MustRead(ctx, cmd, s, nil).Framebuffer()
		read(ctx, bh, vb.toVkHandle(uint64(vkFb)))
		if _, ok := vb.commandBuffers[cmd.CommandBuffer()]; ok {
			write(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].renderPassBegin)
		}
		rp := GetState(s).RenderPasses().Get(vkRp)
		fb := GetState(s).Framebuffers().Get(vkFb)
		read(ctx, bh, vb.toVkHandle(uint64(fb.RenderPass().VulkanHandle())))
		for _, ia := range fb.ImageAttachments().All() {
			if read(ctx, bh, vb.toVkHandle(uint64(ia.VulkanHandle()))) {
				read(ctx, bh, vb.toVkHandle(uint64(ia.Image().VulkanHandle())))
			}
		}
		if cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer()); cbc != nil {
			cbc.behave = func(sc submittedCommand,
				execInfo *queueExecutionState) {
				cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
				execInfo.beginRenderPass(ctx, vb, cbh, rp, fb)
				execInfo.renderPassBegin = newForwardPairedLabel(ctx, cbh)
				ft.AddBehavior(ctx, cbh)
				cbh.Alive = true // TODO(awoloszyn)(BUG:1158): Investigate why this is needed.
				// Without this, we drop some needed commands.
			}
		}

	case *VkCmdNextSubpass:
		cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
		cbc.behave = func(sc submittedCommand,
			execInfo *queueExecutionState) {
			cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
			execInfo.nextSubpass(ctx, ft, cbh, sc)
			ft.AddBehavior(ctx, cbh)
			cbh.Alive = true // TODO(awoloszyn)(BUG:1158): Investigate why this is needed.
			// Without this, we drop some needed commands.
		}

	case *VkCmdEndRenderPass:
		if _, ok := vb.commandBuffers[cmd.CommandBuffer()]; ok {
			read(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].renderPassBegin)
			cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
			cbc.behave = func(sc submittedCommand,
				execInfo *queueExecutionState) {
				cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
				execInfo.endRenderPass(ctx, ft, cbh, sc)
				read(ctx, cbh, execInfo.renderPassBegin)
				ft.AddBehavior(ctx, cbh)
				cbh.Alive = true // TODO(awoloszyn)(BUG:1158): Investigate why this is needed.
				// Without this, we drop some needed commands.
			}
		}

	// bind vertex buffers, index buffer, pipeline and descriptors
	case *VkCmdBindVertexBuffers:
		count := uint64(cmd.BindingCount())
		offsets := cmd.POffsets().Slice(0, count, l).MustRead(ctx, cmd, s, nil)
		subBindings := []resBindingList{}
		for i, vkBuf := range cmd.PBuffers().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			subBindings = append(subBindings, vb.buffers[vkBuf].getSubBindingList(ctx, bh,
				uint64(offsets[i]), vkWholeSize))
		}
		firstBinding := cmd.FirstBinding()
		cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
		cbc.behave = func(sc submittedCommand,
			execInfo *queueExecutionState) {
			cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
			for i, sb := range subBindings {
				binding := firstBinding + uint32(i)
				execInfo.currentCmdBufState.vertexBufferResBindings[binding] = sb
			}
			ft.AddBehavior(ctx, cbh)
		}
	case *VkCmdBindIndexBuffer:
		subBindings := vb.buffers[cmd.Buffer()].getSubBindingList(ctx, bh,
			uint64(cmd.Offset()), vkWholeSize)
		cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
		cbc.behave = func(sc submittedCommand,
			execInfo *queueExecutionState) {
			cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
			execInfo.currentCmdBufState.indexBufferResBindings = subBindings
			execInfo.currentCmdBufState.indexType = cmd.IndexType()
			ft.AddBehavior(ctx, cbh)
		}
	case *VkCmdBindPipeline:
		vkPi := cmd.Pipeline()
		read(ctx, bh, vb.toVkHandle(uint64(vkPi)))
		cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
		cbc.behave = func(sc submittedCommand,
			execInfo *queueExecutionState) {
			cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
			read(ctx, cbh, vb.toVkHandle(uint64(vkPi)))
			write(ctx, cbh, execInfo.currentCmdBufState.pipeline)
			ft.AddBehavior(ctx, cbh)
		}
	case *VkCmdBindDescriptorSets:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Layout())))
		count := uint64(cmd.DescriptorSetCount())
		dss := make([]*descriptorSet, 0, count)
		for _, vkSet := range cmd.PDescriptorSets().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			read(ctx, bh, vb.toVkHandle(uint64(vkSet)))
			dss = append(dss, vb.descriptorSets[vkSet])
		}
		firstSet := cmd.FirstSet()
		dOffsets := []uint32{}
		if cmd.DynamicOffsetCount() > uint32(0) {
			dOffsets = cmd.PDynamicOffsets().Slice(0, uint64(cmd.DynamicOffsetCount()),
				l).MustRead(ctx, cmd, s, nil)
		}
		cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
		cbc.behave = func(sc submittedCommand,
			execInfo *queueExecutionState) {
			cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
			for i, ds := range dss {
				set := firstSet + uint32(i)
				execInfo.currentCmdBufState.descriptorSets[set] = newBoundDescriptorSet(ctx, cbh, ds, dOffsets)
			}
			ft.AddBehavior(ctx, cbh)
		}

	// draw and dispatch
	case *VkCmdDraw:
		if _, ok := vb.commandBuffers[cmd.CommandBuffer()]; ok {
			read(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].renderPassBegin)
			cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
			cbc.behave = func(sc submittedCommand,
				execInfo *queueExecutionState) {
				cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
				vb.draw(ctx, cbh, execInfo)
				ft.AddBehavior(ctx, cbh)
			}
		}

	case *VkCmdDrawIndexed:
		if _, ok := vb.commandBuffers[cmd.CommandBuffer()]; ok {
			read(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].renderPassBegin)
			cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
			cbc.behave = func(sc submittedCommand,
				execInfo *queueExecutionState) {
				cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
				vb.readBoundIndexBuffer(ctx, cbh, execInfo, cmd)
				vb.draw(ctx, cbh, execInfo)
				ft.AddBehavior(ctx, cbh)
			}
		}

	case *VkCmdDrawIndirect:
		if _, ok := vb.commandBuffers[cmd.CommandBuffer()]; ok {
			read(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].renderPassBegin)
		}
		count := uint64(cmd.DrawCount())
		sizeOfDrawIndirectdCommand := uint64(4 * 4)
		offset := uint64(cmd.Offset())
		src := []dependencygraph.DefUseVariable{}
		for i := uint64(0); i < count; i++ {
			src = append(src, vb.getBufferData(ctx, bh, cmd.Buffer(), offset,
				sizeOfDrawIndirectdCommand)...)
			offset += uint64(cmd.Stride())
		}
		if cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer()); cbc != nil {
			cbc.behave = func(sc submittedCommand,
				execInfo *queueExecutionState) {
				cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
				vb.draw(ctx, cbh, execInfo)
				read(ctx, cbh, src...)
				ft.AddBehavior(ctx, cbh)
			}
		}

	case *VkCmdDrawIndexedIndirect:
		if _, ok := vb.commandBuffers[cmd.CommandBuffer()]; ok {
			read(ctx, bh, vb.commandBuffers[cmd.CommandBuffer()].renderPassBegin)
		}
		count := uint64(cmd.DrawCount())
		sizeOfDrawIndexedIndirectCommand := uint64(5 * 4)
		offset := uint64(cmd.Offset())
		src := []dependencygraph.DefUseVariable{}
		for i := uint64(0); i < count; i++ {
			src = append(src, vb.getBufferData(ctx, bh, cmd.Buffer(), offset,
				sizeOfDrawIndexedIndirectCommand)...)
			offset += uint64(cmd.Stride())
		}
		if cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer()); cbc != nil {
			cbc.behave = func(sc submittedCommand,
				execInfo *queueExecutionState) {
				cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
				vb.readBoundIndexBuffer(ctx, cbh, execInfo, cmd)
				vb.draw(ctx, cbh, execInfo)
				read(ctx, cbh, src...)
				ft.AddBehavior(ctx, cbh)
			}
		}

	case *VkCmdDispatch:
		cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
		cbc.behave = func(sc submittedCommand,
			execInfo *queueExecutionState) {
			cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
			read(ctx, cbh, execInfo.currentCmdBufState.pipeline)
			modified := vb.useBoundDescriptorSets(ctx, cbh, execInfo.currentCmdBufState)
			modify(ctx, cbh, modified...)
			ft.AddBehavior(ctx, cbh)
		}

	case *VkCmdDispatchIndirect:
		sizeOfDispatchIndirectCommand := uint64(3 * 4)
		src := vb.getBufferData(ctx, bh, cmd.Buffer(), uint64(cmd.Offset()), sizeOfDispatchIndirectCommand)
		cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
		cbc.behave = func(sc submittedCommand,
			execInfo *queueExecutionState) {
			cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
			read(ctx, cbh, execInfo.currentCmdBufState.pipeline)
			modified := vb.useBoundDescriptorSets(ctx, cbh, execInfo.currentCmdBufState)
			modify(ctx, cbh, modified...)
			read(ctx, cbh, src...)
			ft.AddBehavior(ctx, cbh)
		}

	// pipeline settings
	case *VkCmdPushConstants:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Layout())))
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdSetLineWidth:
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdSetScissor:
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdSetViewport:
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdSetDepthBias:
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdSetDepthBounds:
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdSetBlendConstants:
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdSetStencilCompareMask:
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdSetStencilWriteMask:
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdSetStencilReference:
		vb.recordModifingDynamicStates(ctx, ft, bh, cmd.CommandBuffer())

	// clear attachments
	case *VkCmdClearAttachments:
		attCount := uint64(cmd.AttachmentCount())
		atts := make([]VkClearAttachment, 0, attCount)
		rectCount := uint64(cmd.RectCount())
		rects := make([]VkClearRect, 0, rectCount)
		for _, att := range cmd.PAttachments().Slice(0, attCount, l).MustRead(ctx, cmd, s, nil) {
			atts = append(atts, att)
		}
		for _, rect := range cmd.PRects().Slice(0, rectCount, l).MustRead(ctx, cmd, s, nil) {
			rects = append(rects, rect)
		}
		cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
		cbc.behave = func(sc submittedCommand,
			execInfo *queueExecutionState) {
			cbh := sc.cmd.newBehavior(ctx, sc, execInfo)
			for _, a := range atts {
				clearAttachmentData(ctx, cbh, execInfo, a, rects)
			}
			ft.AddBehavior(ctx, cbh)
		}

	// query pool commands
	case *VkCmdResetQueryPool:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.QueryPool())))
		resetLabels := []dependencygraph.DefUseVariable{}
		count := uint64(cmd.QueryCount())
		first := uint64(cmd.FirstQuery())
		for i := uint64(0); i < count; i++ {
			resetLabels = append(resetLabels,
				vb.querypools[cmd.QueryPool()].queries[first+i].reset)
		}
		vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(), emptyDefUseVars,
			resetLabels, emptyDefUseVars)
	case *VkCmdBeginQuery:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.QueryPool())))
		resetLabels := []dependencygraph.DefUseVariable{
			vb.querypools[cmd.QueryPool()].queries[cmd.Query()].reset}
		beginLabels := []dependencygraph.DefUseVariable{
			vb.querypools[cmd.QueryPool()].queries[cmd.Query()].begin}
		vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(), resetLabels,
			beginLabels, emptyDefUseVars)
	case *VkCmdEndQuery:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.QueryPool())))
		endAndResultLabels := []dependencygraph.DefUseVariable{
			vb.querypools[cmd.QueryPool()].queries[cmd.Query()].end,
			vb.querypools[cmd.QueryPool()].queries[cmd.Query()].result,
		}
		beginLabels := []dependencygraph.DefUseVariable{
			vb.querypools[cmd.QueryPool()].queries[cmd.Query()].begin}
		vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(), beginLabels,
			endAndResultLabels, emptyDefUseVars)
	case *VkCmdWriteTimestamp:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.QueryPool())))
		resetLabels := []dependencygraph.DefUseVariable{
			vb.querypools[cmd.QueryPool()].queries[cmd.Query()].reset}
		resultLabels := []dependencygraph.DefUseVariable{
			vb.querypools[cmd.QueryPool()].queries[cmd.Query()].result}
		vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(), resetLabels,
			resultLabels, emptyDefUseVars)
	case *VkCmdCopyQueryPoolResults:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.QueryPool())))
		// TODO: calculate the range
		src := []dependencygraph.DefUseVariable{}
		dst := vb.getBufferData(ctx, bh, cmd.DstBuffer(), 0, vkWholeSize)
		count := uint64(cmd.QueryCount())
		first := uint64(cmd.FirstQuery())
		for i := uint64(0); i < count; i++ {
			src = append(src, vb.querypools[cmd.QueryPool()].queries[first+i].result)
		}
		vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(), src, emptyDefUseVars, dst)

	// debug marker extension commandbuffer commands. Those commands are kept
	// alive if they are submitted.
	case *VkCmdDebugMarkerBeginEXT:
		vb.keepSubmittedCommandAlive(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdDebugMarkerEndEXT:
		vb.keepSubmittedCommandAlive(ctx, ft, bh, cmd.CommandBuffer())
	case *VkCmdDebugMarkerInsertEXT:
		vb.keepSubmittedCommandAlive(ctx, ft, bh, cmd.CommandBuffer())

	// event commandbuffer commands
	case *VkCmdSetEvent:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Event())))
		vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(), emptyDefUseVars,
			[]dependencygraph.DefUseVariable{vb.events[cmd.Event()].signal}, emptyDefUseVars)
	case *VkCmdResetEvent:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Event())))
		vb.recordReadsWritesModifies(ctx, ft, bh, cmd.CommandBuffer(), emptyDefUseVars,
			[]dependencygraph.DefUseVariable{vb.events[cmd.Event()].unsignal}, emptyDefUseVars)
	case *VkCmdWaitEvents:
		evCount := uint64(cmd.EventCount())
		eventLabels := make([]dependencygraph.DefUseVariable, 0, evCount*uint64(2))
		for _, vkEv := range cmd.PEvents().Slice(0, evCount, l).MustRead(ctx, cmd, s, nil) {
			read(ctx, bh, vb.toVkHandle(uint64(vkEv)))
			eventLabels = append(eventLabels, vb.events[vkEv].signal,
				vb.events[vkEv].unsignal)
		}
		vb.recordBarriers(ctx, s, ft, cmd, bh, cmd.CommandBuffer(), cmd.MemoryBarrierCount(),
			cmd.BufferMemoryBarrierCount(), cmd.PBufferMemoryBarriers(),
			cmd.ImageMemoryBarrierCount(), cmd.PImageMemoryBarriers(), eventLabels)

	// pipeline barrier
	case *VkCmdPipelineBarrier:
		vb.recordBarriers(ctx, s, ft, cmd, bh, cmd.CommandBuffer(), cmd.MemoryBarrierCount(),
			cmd.BufferMemoryBarrierCount(), cmd.PBufferMemoryBarriers(),
			cmd.ImageMemoryBarrierCount(), cmd.PImageMemoryBarriers(), emptyDefUseVars)

	// secondary command buffers
	case *VkCmdExecuteCommands:
		cbc := vb.newCommand(ctx, bh, cmd.CommandBuffer())
		cbc.isCmdExecuteCommands = true
		count := uint64(cmd.CommandBufferCount())
		for _, vkScb := range cmd.PCommandBuffers().Slice(0, count, l).MustRead(ctx, cmd, s, nil) {
			cbc.recordSecondaryCommandBuffer(vkScb)
			read(ctx, bh, vb.toVkHandle(uint64(vkScb)))
		}
		cbc.behave = func(sc submittedCommand, execInfo *queueExecutionState) {}

	// execution triggering
	case *VkQueueSubmit:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Queue())))
		if _, ok := vb.executionStates[cmd.Queue()]; !ok {
			vb.executionStates[cmd.Queue()] = newQueueExecutionState(id)
		}
		vb.executionStates[cmd.Queue()].lastSubmitID = id
		// collect submission info and submitted commands
		vb.submitInfos[id] = &queueSubmitInfo{
			began:  false,
			queued: newLabel(),
			done:   newLabel(),
			queue:  cmd.Queue(),
		}
		submitCount := uint64(cmd.SubmitCount())
		hasCmd := false
		for i, submit := range cmd.PSubmits().Slice(0, submitCount, l).MustRead(ctx, cmd, s, nil) {
			commandBufferCount := uint64(submit.CommandBufferCount())
			for j := uint64(0); j < commandBufferCount; j++ {
				vkCb := submit.PCommandBuffers().Slice(j, j+1, l).MustRead(ctx, cmd, s, nil)[0]
				// In case of invalid command buffer handle, stop traversing the whole
				// slice.
				if _, ok := vb.commandBuffers[vkCb]; !ok {
					break
				}
				read(ctx, bh, vb.commandBuffers[vkCb].end)
				for k, cbc := range vb.commands[vkCb] {
					if !hasCmd {
						hasCmd = true
					}
					fci := api.SubCmdIdx{uint64(id), uint64(i), uint64(j), uint64(k)}
					submittedCmd := newSubmittedCommand(fci, cbc, nil)
					vb.submitInfos[id].pendingCommands = append(vb.submitInfos[id].pendingCommands, submittedCmd)
					if cbc.isCmdExecuteCommands {
						for scbi, scb := range cbc.secondaryCommandBuffers {
							// In case of invalid secondary command buffer, stop traversing
							// all the secondary command buffers
							if _, ok := vb.commandBuffers[scb]; !ok {
								break
							}
							read(ctx, bh, vb.commandBuffers[scb].end)
							for sci, scbc := range vb.commands[scb] {
								fci := api.SubCmdIdx{uint64(id), uint64(i), uint64(j), uint64(k), uint64(scbi), uint64(sci)}
								submittedCmd := newSubmittedCommand(fci, scbc, cbc)
								vb.submitInfos[id].pendingCommands = append(vb.submitInfos[id].pendingCommands, submittedCmd)
							}
						}
					}
				}
			}
			waitSemaphoreCount := uint64(submit.WaitSemaphoreCount())
			for j := uint64(0); j < waitSemaphoreCount; j++ {
				sp := submit.PWaitSemaphores().Slice(j, j+1, l).MustRead(ctx, cmd, s, nil)[0]
				// In case of invalid semaphores, stop traversing all the semaphores.
				if !GetState(s).Semaphores().Contains(sp) {
					break
				}
				vb.submitInfos[id].waitSemaphores = append(vb.submitInfos[id].waitSemaphores, sp)
			}
			signalSemaphoreCount := uint64(submit.SignalSemaphoreCount())
			for j := uint64(0); j < signalSemaphoreCount; j++ {
				sp := submit.PSignalSemaphores().Slice(j, j+1, l).MustRead(ctx, cmd, s, nil)[0]
				// In case of invalid semaphores, stop traversing all the semaphores.
				if !GetState(s).Semaphores().Contains(sp) {
					break
				}
				vb.submitInfos[id].signalSemaphores = append(vb.submitInfos[id].signalSemaphores, sp)
			}
		}
		vb.submitInfos[id].signalFence = cmd.Fence()

		// queue execution begin
		vb.writeCoherentMemoryData(ctx, cmd, bh)
		if read(ctx, bh, vb.toVkHandle(uint64(cmd.Fence()))) {
			read(ctx, bh, vb.fences[cmd.Fence()].unsignal)
			write(ctx, bh, vb.fences[cmd.Fence()].signal)
		}
		// If the submission does not contains commands, records the write
		// behavior here as we don't have any callbacks for those operations.
		// This is not exactly correct. If the whole submission is in pending
		// state, even if there is no command to submit, those semaphore/fence
		// signal/unsignal operations will be in pending, instead of being
		// carried out immediately.
		// TODO: Once we merge the dependency tree building process to mutate
		// calls, make sure the signal/unsignal operations in pending state
		// are handled correctly.
		write(ctx, bh, vb.submitInfos[id].queued)
		for _, sp := range vb.submitInfos[id].waitSemaphores {
			if read(ctx, bh, vb.toVkHandle(uint64(sp))) {
				if !hasCmd {
					modify(ctx, bh, vb.semaphoreSignals[sp])
				}
			}
		}
		for _, sp := range vb.submitInfos[id].signalSemaphores {
			if read(ctx, bh, vb.toVkHandle(uint64(sp))) {
				if !hasCmd {
					write(ctx, bh, vb.toVkHandle(uint64(sp)))
				}
			}
		}
		if read(ctx, bh, vb.toVkHandle(uint64(cmd.Fence()))) {
			if !hasCmd {
				write(ctx, bh, vb.fences[cmd.Fence()].signal)
			}
		}

	case *VkSetEvent:
		if read(ctx, bh, vb.toVkHandle(uint64(cmd.Event()))) {
			write(ctx, bh, vb.events[cmd.Event()].signal)
			vb.writeCoherentMemoryData(ctx, cmd, bh)
			bh.Alive = true
		}

	case *VkQueueBindSparse:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Queue())))
		for _, bindInfo := range cmd.PBindInfo().Slice(0, uint64(cmd.BindInfoCount()), l).MustRead(
			ctx, cmd, s, nil) {
			for _, bufferBinds := range bindInfo.PBufferBinds().Slice(0,
				uint64(bindInfo.BufferBindCount()), l).MustRead(ctx, cmd, s, nil) {
				if read(ctx, bh, vb.toVkHandle(uint64(bufferBinds.Buffer()))) {
					buf := bufferBinds.Buffer()
					binds := bufferBinds.PBinds().Slice(0, uint64(bufferBinds.BindCount()), l).MustRead(
						ctx, cmd, s, nil)
					for _, bind := range binds {
						if read(ctx, bh, vb.toVkHandle(uint64(bind.Memory()))) {
							vb.addBufferMemBinding(ctx, bh, buf, bind.Memory(),
								uint64(bind.ResourceOffset()), uint64(bind.Size()), uint64(bind.MemoryOffset()))
						}
					}
				}
			}
			for _, opaqueBinds := range bindInfo.PImageOpaqueBinds().Slice(0,
				uint64(bindInfo.ImageOpaqueBindCount()), l).MustRead(ctx, cmd, s, nil) {
				if read(ctx, bh, vb.toVkHandle(uint64(opaqueBinds.Image()))) {
					img := opaqueBinds.Image()
					binds := opaqueBinds.PBinds().Slice(0, uint64(opaqueBinds.BindCount()), l).MustRead(
						ctx, cmd, s, nil)
					for _, bind := range binds {
						if read(ctx, bh, vb.toVkHandle(uint64(bind.Memory()))) {
							vb.addOpaqueImageMemBinding(ctx, bh, img, bind.Memory(),
								uint64(bind.ResourceOffset()), uint64(bind.Size()), uint64(bind.MemoryOffset()))
						}
					}
				}
			}
			for _, imageBinds := range bindInfo.PImageBinds().Slice(0,
				uint64(bindInfo.ImageBindCount()), l).MustRead(ctx, cmd, s, nil) {
				if read(ctx, bh, vb.toVkHandle(uint64(imageBinds.Image()))) {
					img := imageBinds.Image()
					binds := imageBinds.PBinds().Slice(0, uint64(imageBinds.BindCount()), l).MustRead(
						ctx, cmd, s, nil)
					for _, bind := range binds {
						if read(ctx, bh, vb.toVkHandle(uint64(bind.Memory()))) {
							vb.addSparseImageMemBinding(ctx, cmd, id, s, bh, img, bind)
						}
					}
				}
			}
		}

	// synchronization primitives
	case *VkResetEvent:
		if read(ctx, bh, vb.toVkHandle(uint64(cmd.Event()))) {
			write(ctx, bh, vb.events[cmd.Event()].unsignal)
			bh.Alive = true
		}

	case *VkCreateSemaphore:
		vkSp := cmd.PSemaphore().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(vkSp)))
		vb.semaphoreSignals[vkSp] = newLabel()
	case *VkDestroySemaphore:
		vkSp := cmd.Semaphore()
		if read(ctx, bh, vb.toVkHandle(uint64(vkSp))) {
			delete(vb.semaphoreSignals, vkSp)
			bh.Alive = true
		}

	case *VkCreateEvent:
		vkEv := cmd.PEvent().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(vkEv)))
		vb.events[vkEv] = &event{signal: newLabel(), unsignal: newLabel()}
	case *VkGetEventStatus:
		vkEv := cmd.Event()
		if read(ctx, bh, vb.toVkHandle(uint64(vkEv))) {
			read(ctx, bh, vb.events[vkEv].signal)
			read(ctx, bh, vb.events[vkEv].unsignal)
			bh.Alive = true
		}
	case *VkDestroyEvent:
		vkEv := cmd.Event()
		if read(ctx, bh, vb.toVkHandle(uint64(vkEv))) {
			delete(vb.events, vkEv)
			bh.Alive = true
		}

	case *VkCreateFence:
		vkFe := cmd.PFence().MustRead(ctx, cmd, s, nil)
		write(ctx, bh, vb.toVkHandle(uint64(vkFe)))
		vb.fences[vkFe] = &fence{signal: newLabel(), unsignal: newLabel()}
	case *VkGetFenceStatus:
		vkFe := cmd.Fence()
		if read(ctx, bh, vb.toVkHandle(uint64(vkFe))) {
			read(ctx, bh, vb.fences[vkFe].signal)
			read(ctx, bh, vb.fences[vkFe].unsignal)
			bh.Alive = true
		}
	case *VkWaitForFences:
		fenceCount := uint64(cmd.FenceCount())
		for _, vkFe := range cmd.PFences().Slice(0, fenceCount, l).MustRead(ctx, cmd, s, nil) {
			if read(ctx, bh, vb.toVkHandle(uint64(vkFe))) {
				read(ctx, bh, vb.fences[vkFe].signal)
				read(ctx, bh, vb.fences[vkFe].unsignal)
				bh.Alive = true
			}
		}
	case *VkResetFences:
		fenceCount := uint64(cmd.FenceCount())
		for _, vkFe := range cmd.PFences().Slice(0, fenceCount, l).MustRead(ctx, cmd, s, nil) {
			if read(ctx, bh, vb.toVkHandle(uint64(vkFe))) {
				write(ctx, bh, vb.fences[vkFe].unsignal)
				bh.Alive = true
			}
		}
	case *VkDestroyFence:
		vkFe := cmd.Fence()
		if read(ctx, bh, vb.toVkHandle(uint64(vkFe))) {
			delete(vb.fences, vkFe)
			bh.Alive = true
		}

	case *VkQueueWaitIdle:
		vkQu := cmd.Queue()
		if read(ctx, bh, vb.toVkHandle(uint64(vkQu))) {
			if _, ok := vb.executionStates[vkQu]; ok {
				bh.Alive = true
			}
		}

	case *VkDeviceWaitIdle:
		for _, qei := range vb.executionStates {
			lastSubmitInfo := vb.submitInfos[qei.lastSubmitID]
			read(ctx, bh, lastSubmitInfo.done)
			bh.Alive = true
		}

	// Property queries, can be dropped if they are not the requested command.
	case *VkGetDeviceMemoryCommitment:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Memory())))
	case *VkGetImageSubresourceLayout:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.Image())))
	case *VkGetRenderAreaGranularity:
		read(ctx, bh, vb.toVkHandle(uint64(cmd.RenderPass())))
	case *VkEnumerateInstanceExtensionProperties,
		*VkEnumerateDeviceExtensionProperties,
		*VkEnumerateInstanceLayerProperties,
		*VkEnumerateDeviceLayerProperties:

	// Keep alive
	case *VkGetDeviceProcAddr,
		*VkGetInstanceProcAddr:
		bh.Alive = true
	case *VkCreateInstance:
		bh.Alive = true
	case *VkEnumeratePhysicalDevices:
		bh.Alive = true
	case *VkCreateDevice:
		bh.Alive = true
	case *VkGetDeviceQueue:
		bh.Alive = true
	case *VkCreateDescriptorPool,
		*VkDestroyDescriptorPool,
		*VkResetDescriptorPool:
		bh.Alive = true
	case *VkCreateAndroidSurfaceKHR,
		*VkCreateXlibSurfaceKHR,
		*VkCreateXcbSurfaceKHR,
		*VkCreateWaylandSurfaceKHR,
		*VkCreateMirSurfaceKHR,
		*VkCreateWin32SurfaceKHR,
		*VkDestroySurfaceKHR:
		bh.Alive = true
	case *VkCreateCommandPool,
		// TODO: ResetCommandPool should overwrite all the command buffers in this
		// pool.
		*VkResetCommandPool,
		*VkTrimCommandPool,
		*VkTrimCommandPoolKHR,
		*VkDestroyCommandPool:
		bh.Alive = true
	case *VkGetPhysicalDeviceXlibPresentationSupportKHR,
		*VkGetPhysicalDeviceXcbPresentationSupportKHR,
		*VkGetPhysicalDeviceWaylandPresentationSupportKHR,
		*VkGetPhysicalDeviceWin32PresentationSupportKHR,
		*VkGetPhysicalDeviceMirPresentationSupportKHR:
		bh.Alive = true
	case *VkGetPhysicalDeviceProperties,
		*VkGetPhysicalDeviceMemoryProperties,
		*VkGetPhysicalDeviceQueueFamilyProperties,
		*VkGetPhysicalDeviceDisplayPropertiesKHR,
		*VkGetPhysicalDeviceDisplayPlanePropertiesKHR,
		*VkGetPhysicalDeviceFeatures,
		*VkGetPhysicalDeviceFormatProperties,
		*VkGetPhysicalDeviceImageFormatProperties,
		*VkGetPhysicalDeviceSparseImageFormatProperties:
		bh.Alive = true
	case *VkGetPhysicalDeviceSurfaceSupportKHR,
		*VkGetPhysicalDeviceSurfaceCapabilitiesKHR,
		*VkGetPhysicalDeviceSurfaceFormatsKHR,
		*VkGetPhysicalDeviceSurfacePresentModesKHR:
		bh.Alive = true
	case *VkGetDisplayPlaneSupportedDisplaysKHR,
		*VkGetDisplayModePropertiesKHR,
		*VkGetDisplayPlaneCapabilitiesKHR,
		*VkCreateDisplayPlaneSurfaceKHR,
		*VkCreateDisplayModeKHR:
		bh.Alive = true
	// Unhandled, always keep alive
	default:
		log.W(ctx, "Command: %v is not handled in FootprintBuilder", cmd)
		bh.Alive = true
	}

	ft.AddBehavior(ctx, bh)

	// roll out the recorded reads and writes for queue submit and set event
	switch cmd.(type) {
	case *VkQueueSubmit:
		vb.rollOutExecuted(ctx, ft, executedCommands)
	case *VkSetEvent:
		vb.rollOutExecuted(ctx, ft, executedCommands)
	}

	// Records the current last draw framebuffer image data, so that later when
	// the user request a command, we can always guarantee that the last draw
	// framebuffer is alive.
	fbData := []dependencygraph.DefUseVariable{}
	if GetState(s).LastSubmission() == LastSubmissionType_SUBMIT {
		lastQueue := GetState(s).LastBoundQueue()
		if !lastQueue.IsNil() && GetState(s).LastDrawInfos().Contains(lastQueue.VulkanHandle()) {
			lastDraw := GetState(s).LastDrawInfos().Get(lastQueue.VulkanHandle())
			if !lastDraw.Framebuffer().IsNil() {
				for _, view := range lastDraw.Framebuffer().ImageAttachments().All() {
					if view.IsNil() || view.Image().IsNil() {
						continue
					}
					img := view.Image()
					fbData = append(fbData, vb.getImageData(ctx, nil, img.VulkanHandle())...)
				}
			}
		}
	}
	if len(fbData) > 0 {
		fbh := dependencygraph.NewBehavior(api.SubCmdIdx{uint64(id)})
		read(ctx, fbh, fbData...)
		ft.AddBehavior(ctx, fbh)
	}
}

func (vb *FootprintBuilder) writeCoherentMemoryData(ctx context.Context,
	cmd api.Cmd, bh *dependencygraph.Behavior) {
	if cmd.Extras() == nil || cmd.Extras().Observations() == nil {
		return
	}
	for _, ro := range cmd.Extras().Observations().Reads {
		// Here we intersect all the memory observations with all the mapped
		// coherent memories. If any intersects are found, mark the behavior
		// as alive (explained in the loop below).
		// Another more intuitive way is to cache the observation here then, pull
		// the data later when rolling out the submitted commands, this way we only
		// record 'write' operation for the observations that are actually used in
		// the submitted commands. But it actually does not help, because without
		// the permit to modify api.Cmd, the coherent memory observations can only
		// be 'alive' or 'dead' altogether. Postponing the recording of 'write'
		// operation does not save any data.
		for vkDm, mm := range vb.mappedCoherentMemories {
			mappedRng := memory.Range{
				Base: uint64(mm.MappedLocation().Address()),
				Size: uint64(mm.MappedSize()),
			}
			if ro.Range.Overlaps(mappedRng) {

				// Dirty hack. If there are coherent memory observation attached on
				// this vkQueueSubmit, we need to keep, even if all the commands in
				// this submission are useless. This is because the observed pages
				// might be shared with other following commands in future queue
				// submissions. As we are not going to modify api.Cmd here to pass the
				// observations, we need those observation being called with
				// ApplyReads(). So we need to keep such vkQueueSubmit. vkUnmapMemory
				// has the same issue.
				bh.Alive = true

				intersect := ro.Range.Intersect(mappedRng)
				offset := uint64(mm.MappedOffset()) + intersect.Base - mm.MappedLocation().Address()
				ms := vb.newMemorySpan(vkDm, offset, intersect.Size)
				write(ctx, bh, ms)
			}
		}
	}
}

// helper functions
func debug(ctx context.Context, fmt string, args ...interface{}) {
	if config.DebugDeadCodeElimination {
		log.D(ctx, fmt, args...)
	}
}

func read(ctx context.Context, bh *dependencygraph.Behavior,
	cs ...dependencygraph.DefUseVariable) bool {
	allSucceeded := true
	for _, c := range cs {
		switch c := c.(type) {
		case *vkHandle:
			if c.isNullHandle() {
				debug(ctx, "Read to VK_NULL_HANDLE is ignored")
				allSucceeded = false
				continue
			}
			bh.Read(c)
		case *forwardPairedLabel:
			// c.GetDefBehavior().DependsOn[bh] = struct{}{}
			c.labelReadBehaviors = append(c.labelReadBehaviors, bh)
			bh.Read(c)
		case *memorySpan:
			if c.memory == VkDeviceMemory(0) {
				continue
			}
			first, count := interval.Intersect(memBindingList(c.recordTo.records[c.memory]), c.span())
			if count > 0 {
				for i := first; i < first+count; i++ {
					sp := c.recordTo.records[c.memory][i].(*memorySpan)
					bh.Read(sp)
				}
			}
		default:
			bh.Read(c)
		}
		debug(ctx, "<Behavior: %v, Read: %v>", bh, c)
	}
	return allSucceeded
}

func write(ctx context.Context, bh *dependencygraph.Behavior,
	cs ...dependencygraph.DefUseVariable) bool {
	allSucceeded := true
	for _, c := range cs {
		switch c := c.(type) {
		case *vkHandle:
			if c.isNullHandle() {
				debug(ctx, "Write to VK_NULL_HANDLE is ignored")
				allSucceeded = false
				continue
			}
			bh.Write(c)
		case *memorySpan:
			if c.memory == VkDeviceMemory(0) {
				continue
			}
			c = c.duplicate().(*memorySpan)
			newList, err := addBinding(memBindingList(c.recordTo.records[c.memory]), c)
			if err != nil {
				debug(ctx, "Adding memory span failed. DeviceMemory: %v, Span: %v", c.memory, c.span())
				allSucceeded = false
				continue
			}
			c.recordTo.records[c.memory] = memorySpanList(newList)
			bh.Write(c)
		default:
			bh.Write(c)
		}
		debug(ctx, "<Behavior: %v, Write: %v>", bh, c)
	}
	return allSucceeded
}

func modify(ctx context.Context, bh *dependencygraph.Behavior,
	cs ...dependencygraph.DefUseVariable) bool {
	allSucceeded := read(ctx, bh, cs...)
	return allSucceeded && write(ctx, bh, cs...)
}

func framebufferPortCoveredByClearRect(fb FramebufferObjectʳ, r VkClearRect) bool {
	if r.BaseArrayLayer() == uint32(0) &&
		r.LayerCount() == fb.Layers() &&
		r.Rect().Offset().X() == 0 && r.Rect().Offset().Y() == 0 &&
		r.Rect().Extent().Width() == fb.Width() &&
		r.Rect().Extent().Height() == fb.Height() {
		return true
	}
	return false
}

func clearAttachmentData(ctx context.Context, bh *dependencygraph.Behavior,
	execInfo *queueExecutionState, a VkClearAttachment, rects []VkClearRect) {
	subpass := &execInfo.subpasses[execInfo.subpass.val]
	if a.AspectMask() == VkImageAspectFlags(VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT) ||
		a.AspectMask() == VkImageAspectFlags(VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT) {
		if subpass.depthStencilAttachment != nil {
			modify(ctx, bh, subpass.depthStencilAttachment.data...)
			return
		}
	} else if a.AspectMask() == VkImageAspectFlags(
		VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT|
			VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT) {
		if subpass.depthStencilAttachment != nil {
			overwritten := false
			for _, r := range rects {
				if framebufferPortCoveredByClearRect(execInfo.framebuffer, r) {
					overwritten = true
				}
			}
			if overwritten && subpass.depthStencilAttachment.fullImageData {
				write(ctx, bh, subpass.depthStencilAttachment.data...)
				return
			}
			modify(ctx, bh, subpass.depthStencilAttachment.data...)
			return
		}
	} else {
		if a.ColorAttachment() != vkAttachmentUnused {
			overwritten := false
			for _, r := range rects {
				if framebufferPortCoveredByClearRect(execInfo.framebuffer, r) {
					overwritten = true
				}
			}
			att := subpass.colorAttachments[a.ColorAttachment()]
			if overwritten && att.fullImageData {
				write(ctx, bh, att.data...)
				return
			}
			modify(ctx, bh, att.data...)
			return
		}
	}
}

func subresourceLayersFullyCoverImage(img ImageObjectʳ, layers VkImageSubresourceLayers,
	offset VkOffset3D, extent VkExtent3D) bool {
	if offset.X() != 0 || offset.Y() != 0 || offset.Z() != 0 {
		return false
	}
	if extent.Width() != img.Info().Extent().Width() ||
		extent.Height() != img.Info().Extent().Height() ||
		extent.Depth() != img.Info().Extent().Depth() {
		return false
	}
	if layers.BaseArrayLayer() != uint32(0) {
		return false
	}
	if layers.LayerCount() != img.Info().ArrayLayers() && layers.LayerCount() != vkRemainingArrayLayers {
		return false
	}
	// Be conservative, only returns true if both the depth and the stencil
	// bits are set.
	if layers.AspectMask() == VkImageAspectFlags(
		VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT|
			VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT) {
		return true
	}
	// For color images, returns true
	if layers.AspectMask() == VkImageAspectFlags(
		VkImageAspectFlagBits_VK_IMAGE_ASPECT_COLOR_BIT) {
		return true
	}
	return false
}

func subresourceRangeFullyCoverImage(img ImageObjectʳ, rng VkImageSubresourceRange) bool {
	if rng.BaseArrayLayer() != 0 || rng.BaseMipLevel() != 0 {
		return false
	}
	if (rng.LayerCount() != img.Info().ArrayLayers() && rng.LayerCount() != vkRemainingArrayLayers) ||
		(rng.LevelCount() != img.Info().MipLevels() && rng.LevelCount() != vkRemainingMipLevels) {
		return false
	}
	// Be conservative, only returns true if both the depth and the stencil bits
	// are set.
	if rng.AspectMask() == VkImageAspectFlags(
		VkImageAspectFlagBits_VK_IMAGE_ASPECT_DEPTH_BIT|
			VkImageAspectFlagBits_VK_IMAGE_ASPECT_STENCIL_BIT) ||
		rng.AspectMask() == VkImageAspectFlags(
			VkImageAspectFlagBits_VK_IMAGE_ASPECT_COLOR_BIT) {
		return true
	}
	return false
}

func blitFullyCoverImage(img ImageObjectʳ, layers VkImageSubresourceLayers,
	offset1 VkOffset3D, offset2 VkOffset3D) bool {

	tmpArena := arena.New()
	defer tmpArena.Dispose()

	if offset1.X() == 0 && offset1.Y() == 0 && offset1.Z() == 0 {
		offset := offset1
		extent := NewVkExtent3D(tmpArena,
			uint32(offset2.X()-offset1.X()),
			uint32(offset2.Y()-offset1.Y()),
			uint32(offset2.Z()-offset1.Z()),
		)
		return subresourceLayersFullyCoverImage(img, layers, offset, extent)
	} else if offset2.X() == 0 && offset2.Y() == 0 && offset2.Z() == 0 {
		offset := offset2
		extent := NewVkExtent3D(tmpArena,
			uint32(offset1.X()-offset2.X()),
			uint32(offset1.Y()-offset2.Y()),
			uint32(offset1.Z()-offset2.Z()),
		)
		return subresourceLayersFullyCoverImage(img, layers, offset, extent)
	} else {
		return false
	}
}

func sparseImageMemoryBindGranularity(ctx context.Context, imgObj ImageObjectʳ,
	bind VkSparseImageMemoryBind) (VkExtent3D, bool) {
	for _, r := range imgObj.SparseMemoryRequirements().All() {
		if r.FormatProperties().AspectMask() == bind.Subresource().AspectMask() {
			return r.FormatProperties().ImageGranularity(), true
		}
	}
	log.E(ctx, "Sparse image granularity not found for VkImage: %v, "+
		"with AspectMask: %v", imgObj.VulkanHandle(), bind.Subresource().AspectMask())
	return VkExtent3D{}, false
}
