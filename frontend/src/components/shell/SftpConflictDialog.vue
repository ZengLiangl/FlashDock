<template>
  <el-dialog
    :model-value="visible"
    title="上传冲突"
    width="480px"
    align-center
    :close-on-click-modal="false"
    @close="resolve('skip', false)"
  >
    <div v-if="item" class="conflict-body">
      <p class="conflict-name">
        <strong>{{ item.fileName }}</strong> 已存在于远端
      </p>
      <div class="conflict-grid">
        <div class="conflict-card">
          <div class="card-title">本地</div>
          <div class="card-row"><span>类型</span><b>{{ item.localIsDir ? '目录' : '文件' }}</b></div>
          <div class="card-row"><span>大小</span><b>{{ sizeLabel(item.localSize, item.localIsDir) }}</b></div>
        </div>
        <div class="conflict-card">
          <div class="card-title">远端</div>
          <div class="card-row"><span>类型</span><b>{{ item.isDir ? '目录' : '文件' }}</b></div>
          <div class="card-row"><span>大小</span><b>{{ sizeLabel(item.remoteSize, item.isDir) }}</b></div>
          <div class="card-row"><span>修改</span><b>{{ mtimeLabel(item.remoteMtime) }}</b></div>
        </div>
      </div>
      <el-checkbox v-if="applyToAllCount > 1" v-model="applyToAll">
        对后续 {{ applyToAllCount - 1 }} 个同类冲突同样处理
      </el-checkbox>
    </div>
    <template #footer>
      <div class="conflict-actions">
        <el-button @click="resolve('skip', applyToAll)">跳过</el-button>
        <el-button @click="resolve('duplicate', applyToAll)">保留两者</el-button>
        <el-button v-if="canMerge" type="primary" plain @click="resolve('merge', applyToAll)">合并</el-button>
        <el-button v-if="canReplace" type="primary" @click="resolve('replace', applyToAll)">覆盖</el-button>
        <el-button v-if="canMerge" type="danger" plain @click="resolve('mirror', applyToAll)">镜像覆盖</el-button>
        <el-button
          v-if="canReplace && applyToAllCount > 1"
          type="danger"
          @click="resolve('replace', true)"
        >
          全部覆盖
        </el-button>
        <el-button
          v-if="canMerge && applyToAllCount > 1"
          type="danger"
          @click="resolve('mirror', true)"
        >
          全部镜像
        </el-button>
      </div>
    </template>
  </el-dialog>
</template>

<script>
import { computed, ref, watch } from 'vue'

export default {
  name: 'SftpConflictDialog',
  props: {
    visible: { type: Boolean, default: false },
    item: { type: Object, default: null },
    applyToAllCount: { type: Number, default: 1 },
    formatSize: { type: Function, required: true },
  },
  emits: ['resolve'],
  setup(props, { emit }) {
    const applyToAll = ref(false)
    watch(() => props.item, () => { applyToAll.value = false })

    const canMerge = computed(() => !!props.item?.localIsDir && !!props.item?.isDir)
    const canReplace = computed(() => {
      if (!props.item) return false
      return !!props.item.localIsDir === !!props.item.isDir
    })

    const sizeLabel = (n, isDir) => (isDir ? '目录' : props.formatSize(n || 0))
    const mtimeLabel = (sec) => (sec ? new Date(sec * 1000).toLocaleString() : '-')

    const resolve = (action, apply) => {
      emit('resolve', { action, applyToAll: !!apply })
    }

    return { applyToAll, canMerge, canReplace, sizeLabel, mtimeLabel, resolve }
  },
}
</script>

<style scoped>
.conflict-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.conflict-name {
  margin: 0;
  font-size: 14px;
  color: var(--app-text-primary, #e6edf3);
}
.conflict-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
}
.conflict-card {
  border: 1px solid var(--app-border, #30363d);
  border-radius: 8px;
  padding: 10px 12px;
  background: color-mix(in srgb, var(--app-panel-bg, #161b22) 80%, transparent);
}
.card-title {
  font-size: 12px;
  font-weight: 600;
  margin-bottom: 8px;
  color: var(--app-text-secondary, #8b949e);
}
.card-row {
  display: flex;
  justify-content: space-between;
  gap: 8px;
  font-size: 12px;
  margin-top: 4px;
}
.card-row span { color: var(--app-text-muted, #6e7681); }
.card-row b { font-weight: 500; color: var(--app-text-primary, #e6edf3); }
.conflict-actions {
  display: flex;
  flex-wrap: wrap;
  justify-content: flex-end;
  gap: 8px;
}
</style>
