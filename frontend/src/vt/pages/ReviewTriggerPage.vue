<template>
  <div class="max-w-xl">
    <h1 class="text-xl sm:text-2xl font-bold text-fg mb-6">New review</h1>

    <form @submit.prevent="submit" class="bg-surface rounded-xl border border-edge p-6">
      <p v-if="error" class="text-sm text-danger mb-4">{{ error }}</p>

      <FormField label="GitHub PR URL">
        <VInput
          v-model="url"
          type="url"
          required
          placeholder="https://github.com/owner/repo/pull/123"
          autofocus
        />
      </FormField>

      <p class="text-sm text-fg-subtle mb-4">
        Paste the URL of the PR you want to review. The project must already be configured
        with matching GitHub coordinates.
      </p>

      <div class="flex justify-end">
        <VButton type="submit" :disabled="loading">{{ loading ? 'Triggering...' : 'Trigger review' }}</VButton>
      </div>
    </form>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import vtApi from '../../api/vt'
import FormField from '../components/FormField.vue'
import VInput from '../components/VInput.vue'
import VButton from '../components/VButton.vue'

const url = ref('')
const loading = ref(false)
const error = ref('')

async function submit() {
  loading.value = true
  error.value = ''
  try {
    const reviewId = await vtApi.review.trigger({ prUrl: url.value })
    window.location.href = `/reviews/${reviewId}/`
  } catch (e: unknown) {
    error.value = e instanceof Error ? e.message : 'Unknown error'
  } finally {
    loading.value = false
  }
}
</script>
