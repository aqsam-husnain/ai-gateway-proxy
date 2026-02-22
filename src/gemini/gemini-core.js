import { OAuth2Client } from 'google-auth-library';
import * as http from 'http';
import * as https from 'https';
import { promises as fs } from 'fs';
import * as path from 'path';
import * as os from 'os';
import * as readline from 'readline';
import open from 'open';
import { API_ACTIONS, formatExpiryTime } from '../common.js';
import { getProviderModels } from '../provider-models.js';
import { handleGeminiCliOAuth } from '../oauth-handlers.js';

// Configure HTTP/HTTPS agent to limit connection pool size to avoid resource leaks
const httpAgent = new http.Agent({
    keepAlive: true,
    maxSockets: 100,
    maxFreeSockets: 5,
    timeout: 120000,
});
const httpsAgent = new https.Agent({
    keepAlive: true,
    maxSockets: 100,
    maxFreeSockets: 5,
    timeout: 120000,
});

// --- Constants ---
const AUTH_REDIRECT_PORT = 8085;
const CREDENTIALS_DIR = '.gemini';
const CREDENTIALS_FILE = 'oauth_creds.json';
const DEFAULT_CODE_ASSIST_ENDPOINT = 'https://cloudcode-pa.googleapis.com';
const DEFAULT_CODE_ASSIST_API_VERSION = 'v1internal';
const OAUTH_CLIENT_ID = process.env.GEMINI_CLI_OAUTH_CLIENT_ID;
const OAUTH_CLIENT_SECRET = process.env.GEMINI_CLI_OAUTH_CLIENT_SECRET;
const GEMINI_MODELS = getProviderModels('gemini-cli-oauth');
const ANTI_TRUNCATION_MODELS = GEMINI_MODELS.map(model => `anti-${model}`);

function is_anti_truncation_model(model) {
    return ANTI_TRUNCATION_MODELS.some(antiModel => model.includes(antiModel));
}

// Extract actual model name from anti-truncation model name
function extract_model_from_anti_model(model) {
    if (model.startsWith('anti-')) {
        const originalModel = model.substring(5); // Remove 'anti-' prefix
        if (GEMINI_MODELS.includes(originalModel)) {
            return originalModel;
        }
    }
    return model; // If not anti- prefix or not in original model list, return original model name
}

function toGeminiApiResponse(codeAssistResponse) {
    if (!codeAssistResponse) return null;
    const compliantResponse = { candidates: codeAssistResponse.candidates };
    if (codeAssistResponse.usageMetadata) compliantResponse.usageMetadata = codeAssistResponse.usageMetadata;
    if (codeAssistResponse.promptFeedback) compliantResponse.promptFeedback = codeAssistResponse.promptFeedback;
    if (codeAssistResponse.automaticFunctionCallingHistory) compliantResponse.automaticFunctionCallingHistory = codeAssistResponse.automaticFunctionCallingHistory;
    return compliantResponse;
}


    isExpiryDateNear() {
        try {
            const currentTime = Date.now();
            const cronNearMinutesInMillis = (this.config.CRON_NEAR_MINUTES || 10) * 60 * 1000;
            console.log(`[Gemini] Expiry date: ${this.authClient.credentials.expiry_date}, Current time: ${currentTime}, ${this.config.CRON_NEAR_MINUTES || 10} minutes from now: ${currentTime + cronNearMinutesInMillis}`);
            return this.authClient.credentials.expiry_date <= (currentTime + cronNearMinutesInMillis);
        } catch (error) {
            console.error(`[Gemini] Error checking expiry date: ${error.message}`);
            return false;
        }
    }

    /**
     * Get model quota information
     * @returns {Promise<Object>} Model quota information
     */
    async getUsageLimits() {
        if (!this.isInitialized) await this.initialize();
        
        // Check if token is near expiry, if so refresh first
        if (this.isExpiryDateNear()) {
            console.log('[Gemini] Token is near expiry, refreshing before getUsageLimits request...');
            await this.initializeAuth(true);
        }

        try {
            const modelsWithQuotas = await this.getModelsWithQuotas();
            return modelsWithQuotas;
        } catch (error) {
            console.error('[Gemini] Failed to get usage limits:', error.message);
            throw error;
        }
    }

    /**
     * Get model list with quota information
     * @returns {Promise<Object>} Model quota information
     */
    async getModelsWithQuotas() {
        try {
            // Parse model quota information
            const result = {
                lastUpdated: Date.now(),
                models: {}
            };

            // Call retrieveUserQuota API to get user quota information
            try {
                const quotaURL = `${this.codeAssistEndpoint}/${this.apiVersion}:retrieveUserQuota`;
                const requestBody = {
                    project: `projects/${this.projectId}`
                };
                const requestOptions = {
                    url: quotaURL,
                    method: 'POST',
                    headers: {
                        'Content-Type': 'application/json'
                    },
                    responseType: 'json',
                    body: JSON.stringify(requestBody)
                };

                const res = await this.authClient.request(requestOptions);
                console.log(`[Gemini] retrieveUserQuota success`);
                if (res.data && res.data.buckets) {
                    const buckets = res.data.buckets;
                    
                    // Iterate through buckets array to extract quota information
                    for (const bucket of buckets) {
                        const modelId = bucket.modelId;
                        
                        // Check if model is in the supported models list
                        if (!GEMINI_MODELS.includes(modelId)) continue;
                        
                        const modelInfo = {
                            remaining: bucket.remainingFraction || 0,
                            resetTime: bucket.resetTime || null,
                            resetTimeRaw: bucket.resetTime
                        };
                        
                        result.models[modelId] = modelInfo;
                    }

                    // Add any missing models from GEMINI_MODELS with default values
                    // Respect notSupportedModels config - don't add models that are disabled
                    const notSupported = this.config?.notSupportedModels || [];
                    for (const modelId of GEMINI_MODELS) {
                        if (!result.models[modelId] && !notSupported.includes(modelId)) {
                            result.models[modelId] = {
                                remaining: 1, // 100% remaining (no usage)
                                resetTime: null,
                                resetTimeRaw: null
                            };
                        }
                    }

                    // Also remove any models from API response that are in notSupportedModels
                    for (const modelId of notSupported) {
                        delete result.models[modelId];
                    }

                    // Sort models by name
                    const sortedModels = {};
                    Object.keys(result.models).sort().forEach(key => {
                        sortedModels[key] = result.models[key];
                    });
                    result.models = sortedModels;
                    console.log(`[Gemini] Successfully fetched quotas for ${Object.keys(result.models).length} models`);
                }
            } catch (fetchError) {
                console.error(`[Gemini] Failed to fetch user quota:`, fetchError.message);

                // If retrieveUserQuota fails, fall back to using fixed model list
                // Respect notSupportedModels config
                const notSupported = this.config?.notSupportedModels || [];
                for (const modelId of GEMINI_MODELS) {
                    if (!notSupported.includes(modelId)) {
                        result.models[modelId] = {
                            remaining: 1, // 100% remaining (unknown usage)
                            resetTime: null,
                            resetTimeRaw: null
                        };
                    }
                }
            }

            return result;
        } catch (error) {
            console.error('[Gemini] Failed to get models with quotas:', error.message);
            throw error;
        }
    }
}
