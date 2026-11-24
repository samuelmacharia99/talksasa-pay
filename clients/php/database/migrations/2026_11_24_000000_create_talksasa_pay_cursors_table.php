<?php

declare(strict_types=1);

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration {
    public function up(): void
    {
        Schema::create(config('talksasa-pay.poller.table', 'talksasa_pay_cursors'), function (Blueprint $table) {
            $table->string('name', 64)->primary();
            $table->string('cursor', 255);
            $table->timestamp('updated_at')->nullable();
        });
    }

    public function down(): void
    {
        Schema::dropIfExists(config('talksasa-pay.poller.table', 'talksasa_pay_cursors'));
    }
};
