// Global Loading UI HTML Helpers
window.getTableLoadingHTML = function(colspan, text = '正在加载数据，请稍候...') {
    return `
        <tr>
            <td colspan="${colspan}" style="padding: 48px 0; text-align: center;">
                <div style="display: inline-flex; flex-direction: column; align-items: center; gap: 12px; justify-content: center; width: 100%;">
                    <div class="loader-spinner"></div>
                    <span style="font-size: 13px; color: var(--slate-400); font-weight: 500;">${text}</span>
                </div>
            </td>
        </tr>
    `;
};

window.getLoadingHTML = function(text = '正在加载...') {
    return `
        <div style="display: inline-flex; align-items: center; gap: 8px; justify-content: center; padding: 16px; width: 100%;">
            <div class="loader-spinner" style="width: 18px; height: 18px; border-width: 2px;"></div>
            <span style="font-size: 13px; color: var(--slate-400); font-weight: 500;">${text}</span>
        </div>
    `;
};

// Global Page Loader helpers
window.showPageLoader = function() {
    const loader = document.getElementById('page-loader');
    if (loader) {
        loader.classList.remove('fade-out');
    }
};

window.hidePageLoader = function() {
    const loader = document.getElementById('page-loader');
    if (loader) {
        loader.classList.add('fade-out');
    }
};

// Global Fetch Interceptor for progress spinner overlay
(function() {
    const originalFetch = window.fetch;
    let activeRequestsCount = 0;

    window.fetch = async function(...args) {
        const url = args[0];
        const isBackground = typeof url === 'string' && (
            url.includes('/api/pay/query') || 
            url.includes('/api/admin/check') ||
            url.includes('/api/admin/logs')
        );

        if (!isBackground) {
            activeRequestsCount++;
            window.showPageLoader();
        }

        try {
            return await originalFetch(...args);
        } catch (error) {
            throw error;
        } finally {
            if (!isBackground) {
                activeRequestsCount--;
                if (activeRequestsCount <= 0) {
                    activeRequestsCount = 0;
                    window.hidePageLoader();
                }
            }
        }
    };
})();

document.addEventListener('DOMContentLoaded', () => {
    // Automatically turn all select elements (except those with .no-custom class) into custom dropdowns
    document.querySelectorAll('select:not(.no-custom)').forEach(select => {
        // Skip if already processed
        if (select.parentNode.classList.contains('custom-select')) return;
        
        try {
            initCustomSelect(select);
        } catch (e) {
            console.error("Failed to wrap custom select dropdown:", select, e);
        }
    });
});

function initCustomSelect(select) {
    if (!select) return;

    // Create wrapper
    const wrapper = document.createElement('div');
    wrapper.className = 'custom-select ' + (select.className || '');
    wrapper.style.minWidth = select.style.minWidth || '150px';

    // Handle full-width selects inside forms dynamically
    const isFullWidth = select.style.width === '100%' || 
                         select.classList.contains('form-select') ||
                         select.classList.contains('form-input') ||
                         select.closest('.form-group') !== null;
    if (isFullWidth) {
        wrapper.style.width = '100%';
        wrapper.style.display = 'block';
    }

    // Hide original select
    select.style.display = 'none';
    select.parentNode.insertBefore(wrapper, select);
    wrapper.appendChild(select);

    // Create trigger
    const trigger = document.createElement('div');
    trigger.className = 'custom-select-trigger';
    
    const triggerText = document.createElement('span');
    const selectedOpt = select.options[select.selectedIndex];
    triggerText.textContent = selectedOpt ? selectedOpt.textContent : '';
    trigger.appendChild(triggerText);

    // Chevron SVG
    trigger.innerHTML += `<svg class="chevron-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg>`;
    wrapper.appendChild(trigger);

    // Create options list container
    const optionsContainer = document.createElement('div');
    optionsContainer.className = 'custom-options-container';
    wrapper.appendChild(optionsContainer);

    // Function to rebuild options dynamically
    function rebuildOptions() {
        optionsContainer.innerHTML = '';
        const currentSelOpt = select.options[select.selectedIndex];
        trigger.querySelector('span').textContent = currentSelOpt ? currentSelOpt.textContent : '';

        Array.from(select.options).forEach((opt, idx) => {
            const customOpt = document.createElement('div');
            customOpt.className = 'custom-option';
            customOpt.textContent = opt.textContent;
            customOpt.setAttribute('data-value', opt.value);
            if (idx === select.selectedIndex) {
                customOpt.classList.add('selected');
            }

            customOpt.addEventListener('click', (e) => {
                e.stopPropagation();
                
                // Update select value
                select.value = opt.value;
                
                // Update trigger text
                trigger.querySelector('span').textContent = opt.textContent;

                // Toggle active class
                optionsContainer.querySelectorAll('.custom-option').forEach(co => {
                    co.classList.remove('selected');
                });
                customOpt.classList.add('selected');

                // Close dropdown
                wrapper.classList.remove('open');

                // Trigger native change event
                select.dispatchEvent(new Event('change'));
            });

            optionsContainer.appendChild(customOpt);
        });
    }

    // Initial build
    rebuildOptions();

    // Observe dynamic changes in select options (e.g. innerHTML updates)
    const observer = new MutationObserver(() => {
        rebuildOptions();
    });
    observer.observe(select, { childList: true, subtree: true });

    // Toggle open/close on trigger click
    trigger.addEventListener('click', (e) => {
        e.stopPropagation();
        const isOpen = wrapper.classList.contains('open');
        
        // Close all other custom dropdowns first
        document.querySelectorAll('.custom-select').forEach(cs => {
            cs.classList.remove('open');
        });

        if (!isOpen) {
            wrapper.classList.add('open');
        }
    });

    // Sync wrapper state if value is modified programmatically
    try {
        const originalVal = Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value');
        if (originalVal && typeof originalVal.get === 'function') {
            Object.defineProperty(select, 'value', {
                get: function() {
                    return originalVal.get.call(this);
                },
                set: function(val) {
                    originalVal.set.call(this, val);
                    const opt = Array.from(select.options).find(o => o.value === val);
                    if (opt) {
                        trigger.querySelector('span').textContent = opt.textContent;
                        optionsContainer.querySelectorAll('.custom-option').forEach(co => {
                            if (co.getAttribute('data-value') === val) {
                                co.classList.add('selected');
                            } else {
                                co.classList.remove('selected');
                            }
                        });
                    }
                }
            });
        }
    } catch (err) {
        console.warn("Failed to intercept select value property:", err);
    }
}

// Close all custom dropdowns on clicking outside
document.addEventListener('click', () => {
    document.querySelectorAll('.custom-select').forEach(cs => {
        cs.classList.remove('open');
    });
});

// Reset page-loader and reload page on bfcache recovery (back-button navigation)
window.addEventListener('pageshow', (event) => {
    const isBackNavigation = event.persisted || 
        (window.performance && window.performance.navigation.type === 2);
    if (isBackNavigation) {
        window.location.reload();
    } else {
        const loader = document.getElementById('page-loader');
        if (loader) {
            loader.classList.add('fade-out');
        }
    }
});
